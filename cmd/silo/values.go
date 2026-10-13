package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/client"
)

// parseAddress reads --address: "/20" (or "20") asks silod to place a
// block of that size, and a CIDR asks for that exact range.
func parseAddress(s string) (*client.AddressWant, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(strings.TrimPrefix(s, "/")); err == nil {
		return &client.AddressWant{PrefixLength: n}, nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return nil, fmt.Errorf("--address %q: want a prefix length (/20) or a range (10.1.16.0/20)", s)
	}
	return &client.AddressWant{Range: p.String()}, nil
}

// parsePrefixLength reads --to: "/19" or "19".
func parsePrefixLength(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "/"))
	if err != nil || n < 1 || n > 32 {
		return 0, fmt.Errorf("%q: want a prefix length such as /19", s)
	}
	return n, nil
}

// globalClasses are the built-in classes with no region, which a
// --quota item may give without one.
var globalClasses = map[string]bool{
	"subnetworks": true, "firewall_rules": true, "images": true, "snapshots": true,
	"image_storage_gb": true, "snapshot_storage_gb": true, "instance_templates": true,
	"gpus_all_regions": true, "agent_sessions": true, "repos": true, "image_repos": true,
}

// classAliases are short names the CLI takes for classes in the class
// table (README walk-through: memory=128GiB, ssd-gb=1000).
var classAliases = map[string]string{
	"memory":      "memory_mb",
	"spot_memory": "spot_memory_mb",
	"ssd_gb":      "ssd_disk_gb",
	"disk_gb":     "standard_disk_gb",
}

// memoryUnits scale an amount of a *_mb class, which counts MiB.
var memoryUnits = map[string]int64{"mib": 1, "mb": 1, "m": 1, "gib": 1024, "gb": 1024, "g": 1024, "tib": 1 << 20, "tb": 1 << 20, "t": 1 << 20}

// parseQuota reads --quota: comma-separated region:class=amount items.
// An item without a region takes the last region given, or "global"
// for a global class. Class names may use - for _, and the aliases
// above; anything else is passed on as it is, since class names are
// open strings (fence-api.md, SV-11) and silod has the table.
func parseQuota(s string) (client.QuotaMap, error) {
	out := client.QuotaMap{}
	region := ""
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		kv, amount, ok := strings.Cut(item, "=")
		if !ok {
			return nil, fmt.Errorf("--quota item %q: want region:class=amount", item)
		}
		r, class, hasRegion := strings.Cut(kv, ":")
		if !hasRegion {
			class, r = r, ""
		}
		class = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(class)), "-", "_")
		if a, ok := classAliases[class]; ok {
			class = a
		}
		switch {
		case hasRegion:
			r = strings.TrimSpace(r)
			if r != "global" {
				region = r
			}
		case globalClasses[class]:
			r = "global"
		case region != "":
			r = region
		default:
			return nil, fmt.Errorf("--quota item %q: give its region, as us-central1:%s=%s", item, class, amount)
		}
		n, err := quotaAmount(class, strings.TrimSpace(amount))
		if err != nil {
			return nil, fmt.Errorf("--quota item %q: %v", item, err)
		}
		if out[r] == nil {
			out[r] = map[string]json.Number{}
		}
		out[r][class] = n
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--quota %q: no items", s)
	}
	return out, nil
}

// quotaAmount reads one amount: a non-negative decimal, which for a
// class counted in MiB may carry a unit (128GiB).
func quotaAmount(class, s string) (json.Number, error) {
	num := s
	scale := int64(1)
	if strings.HasSuffix(class, "_mb") {
		i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
		if i >= 0 {
			u := strings.ToLower(s[i:])
			if memoryUnits[u] == 0 {
				return "", fmt.Errorf("unit %q: want MiB, GiB or TiB", s[i:])
			}
			num, scale = s[:i], memoryUnits[u]
		}
	}
	r, ok := new(big.Rat).SetString(num)
	if !ok || r.Sign() < 0 || strings.ContainsAny(num, "eE/") {
		return "", fmt.Errorf("%q is not a non-negative amount", s)
	}
	r.Mul(r, new(big.Rat).SetInt64(scale))
	if r.IsInt() {
		return json.Number(r.Num().String()), nil
	}
	return json.Number(strings.TrimRight(strings.TrimRight(r.FloatString(6), "0"), ".")), nil
}

// parseMap reads k=v,k=v (labels, grant policy). "" is the empty map.
func parseMap(flagName, s string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--%s item %q: want key=value", flagName, item)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// parseList reads a comma-separated list. "" is the empty list.
func parseList(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseSince reads --since: an RFC 3339 time, a date, or a duration
// back from now (1h, 7d).
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(d); err == nil && n >= 0 {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since %q: want a time (2026-10-05T12:00:00Z), a date, or a duration back from now (90m, 7d)", s)
}

// sortedKeys lists m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
