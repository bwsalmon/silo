package filter

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
)

// objects are a silo's instances as the tenant sees them: names
// without the silo's prefix, and no silo labels.
var objects = decode(`[
 {"name": "web-1", "status": "RUNNING", "zone": "https://compute.googleapis.com/compute/v1/projects/silo-k7f3qa/zones/us-central1-a",
  "id": "1001", "creationTimestamp": "2026-09-01T10:00:00.000-07:00", "cpuPlatform": "Intel Skylake",
  "labels": {"env": "prod", "team": "web"}, "tags": {"items": ["web", "http-server"]},
  "scheduling": {"automaticRestart": true, "preemptible": false},
  "networkInterfaces": [{"networkIP": "10.0.0.2"}, {"networkIP": "10.1.0.2"}],
  "disks": [{"deviceName": "boot", "diskSizeGb": "10"}, {"deviceName": "data", "diskSizeGb": "200"}]},
 {"name": "web-2", "status": "STOPPED", "zone": "https://compute.googleapis.com/compute/v1/projects/silo-k7f3qa/zones/us-central1-b",
  "id": "999", "creationTimestamp": "2026-09-03T10:00:00.000-07:00", "cpuPlatform": "Intel Broadwell",
  "labels": {"env": "dev"}, "tags": {"items": ["web"]},
  "scheduling": {"automaticRestart": false, "preemptible": true},
  "networkInterfaces": [{"networkIP": "10.0.0.3"}],
  "disks": [{"deviceName": "boot", "diskSizeGb": "20"}]},
 {"name": "builder", "status": "RUNNING", "zone": "https://compute.googleapis.com/compute/v1/projects/silo-k7f3qa/zones/us-central1-a",
  "id": "12", "creationTimestamp": "2026-08-20T10:00:00.000-07:00",
  "scheduling": {"automaticRestart": true},
  "networkInterfaces": [{"networkIP": "10.0.0.4"}],
  "guestAccelerators": [], "cpus": 4},
 {"name": "db", "status": "TERMINATED", "zone": "https://compute.googleapis.com/compute/v1/projects/silo-k7f3qa/zones/us-central1-a",
  "id": "13", "creationTimestamp": "2026-09-02T10:00:00.000-07:00",
  "labels": {"env": "prod", "tier": "db"}, "cpus": 16}
]`)

func decode(s string) []map[string]any {
	var out []map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		panic(err)
	}
	return out
}

func matches(t *testing.T, f string) []string {
	t.Helper()
	e, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse(%q): %v", f, err)
	}
	got := []string{}
	for _, o := range objects {
		if e.Match(o) {
			got = append(got, o["name"].(string))
		}
	}
	return got
}

func TestFilter(t *testing.T) {
	for _, c := range []struct {
		source, filter, want string
	}{
		// Captured from gcloud 587.0.0 (see the package doc), with the
		// --filter it was given.
		{"gcloud --filter=name=builder", `name eq ".*\bbuilder\b.*"`, "builder"},
		{"gcloud --filter=name:web", `name eq ".*\bweb\b.*"`, "web-1 web-2"},
		{"gcloud --filter=name~^b.*", `name eq ".*(^b.*).*"`, "builder"},
		{"gcloud --filter='name ~ web-[0-9]+'", `name eq ".*(web-[0-9]+).*"`, "web-1 web-2"},
		{"gcloud --filter=name:web*", `name eq ".*\bweb.*"`, "web-1 web-2"},
		{"gcloud --filter=name=web-1 AND status!=STOPPED", `(name eq ".*\bweb\-1\b.*")(status ne STOPPED)`, "web-1"},
		{"gcloud --filter='status=READY AND name:web'", `(status eq READY)(name eq ".*\bweb\b.*")`, ""},
		{"gcloud --filter='name:web AND NOT status:STOPPED'", `(name eq ".*\bweb\b.*")(status ne STOPPED)`, "web-1"},
		{"gcloud --filter=status=RUNNING", `status eq RUNNING`, "web-1 builder"},
		{"gcloud --filter='NOT status=TERMINATED'", `status ne TERMINATED`, "web-1 web-2 builder"},
		{"gcloud --filter=name!=builder", `name ne ".*\bbuilder\b.*"`, "web-1 web-2 db"},
		{"gcloud --filter=-name:web", `name ne ".*\bweb\b.*"`, "builder db"},
		{"gcloud --filter=zone:us-central1-a", `zone eq ".*\bus\-central1\-a\b.*"`, "web-1 builder db"},
		// A tenant filtering on the real, prefixed name sees nothing.
		{"gcloud --filter=name=k7f3qa-builder", `name eq ".*\bk7f3qa\-builder\b.*"`, ""},

		// Captured from terraform 1.16.5, hashicorp/google 8.5.0.
		{"terraform google_compute_regions status=UP", `(status eq UP)`, ""},
		{"terraform google_compute_zones status=UP", ` (status eq UP)`, ""},
		{"terraform google_compute_images filter", `name != "debian-12" AND labels.env = prod`, "web-1 db"},
		{"terraform google_compute_image filter", `labels.env:* AND (name eq "web-.*")`, "web-1 web-2"},
		{"terraform google_compute_instance_template filter", `name != foo`, "web-1 web-2 builder db"},

		// GCE's documented forms: comparisons, AND, OR, NOT,
		// parentheses, : on labels and lists, nested paths.
		{"empty", ``, "web-1 web-2 builder db"},
		{"=", `name = builder`, "builder"},
		{"= quoted", `cpuPlatform = "Intel Skylake"`, "web-1"},
		{"single quoted", `cpuPlatform = 'Intel Broadwell'`, "web-2"},
		{"OR in parens", `(cpuPlatform = "Intel Skylake") OR (cpuPlatform = "Intel Broadwell")`, "web-1 web-2"},
		{"adjacent", `(scheduling.automaticRestart = true) (cpuPlatform = "Intel Skylake")`, "web-1"},
		{"bool", `scheduling.automaticRestart = false`, "web-2"},
		{"bool case", `scheduling.preemptible = TRUE`, "web-2"},
		{"label", `labels.env = prod`, "web-1 db"},
		{"label present", `labels.env:*`, "web-1 web-2 db"},
		{"label absent", `NOT labels.env:*`, "builder"},
		{"label key", `labels:tier`, "db"},
		{"label wildcard", `labels.env = pr*`, "web-1 db"},
		{"tag", `tags.items:web`, "web-1 web-2"},
		{"tag http", `tags.items = http-server`, "web-1"},
		{"list fan-out", `networkInterfaces.networkIP = 10.1.0.2`, "web-1"},
		{"list fan-out regex", `networkInterfaces.networkIP eq '10\.0\.0\.[34]'`, "web-2 builder"},
		{"list number string", `disks.diskSizeGb > 100`, "web-1"},
		{"empty list present", `guestAccelerators:*`, ""},
		{"!=", `status != RUNNING`, "web-2 db"},
		{"number", `cpus >= 8`, "db"},
		{"number <", `cpus < 8`, "builder"},
		{"number =", `cpus = 16.0`, "db"},
		{"int64 as string", `id > 100`, "web-1 web-2"},
		{"timestamp", `creationTimestamp > "2026-09-01"`, "web-1 web-2 db"},
		{"timestamp <=", `creationTimestamp <= 2026-09-01T10:00:00.000-07:00`, "web-1 builder"},
		{"NOT", `NOT status = RUNNING`, "web-2 db"},
		{"NOT parens", `NOT (status = RUNNING OR status = STOPPED)`, "db"},
		{"- prefix", `-labels.env:*`, "builder"},
		{"AND OR precedence", `status = RUNNING AND name = web-1 OR name = builder`, "web-1 builder"},
		{"OR binds tighter", `name = db OR name = web-2 AND status = STOPPED`, "web-2"},
		{"nested parens", `((name = db) OR (name = builder)) AND cpus > 8`, "db"},
		{"eq unquoted regex", `name eq web-.*`, "web-1 web-2"},
		{"eq whole value", `name eq web`, ""},
		{"ne quoted", `name ne 'web-.*'`, "builder db"},
		{"eq bool", `scheduling.automaticRestart eq true`, "web-1 builder"},
		{"regex parens in bare value", `(name eq (web|db)-?[0-9]*)`, "web-1 web-2 db"},
		{"escaped quote", `cpuPlatform != "Intel \"Skylake\""`, "web-1 web-2 builder db"},
		{"missing field", `nothing = x`, ""},
		{"missing field !=", `nothing != x`, "web-1 web-2 builder db"},
		{"keyword case", `name = builder AND cpus = 4`, "builder"},
		{"hidden silo label", `labels.silo:*`, ""},
		{"real name", `name = k7f3qa-builder`, ""},
		{"real name prefix", `name = k7f3qa-*`, ""},
	} {
		got := strings.Join(matches(t, c.filter), " ")
		if got != c.want {
			t.Errorf("%s: %s matched %q, want %q", c.source, c.filter, got, c.want)
		}
	}
}

func TestInvalid(t *testing.T) {
	for _, f := range []string{
		// gcloud's client-side forms, which it never sends.
		`name ~ web`,
		`name:(web db)`,
		`name.basename()=web`,
		`name = (a OR b)`,
		// Malformed.
		`(name = web`,
		`name = web)`,
		`()`,
		`name =`,
		`name`,
		`= web`,
		`AND name = web`,
		`name = web AND`,
		`name = web OR`,
		`NOT`,
		`name = "web`,
		`name eq "(web"`,
		`labels..env = x`,
		`name == web`,
		`name =< web`,
		`OR = x`,
	} {
		_, err := Parse(f)
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("Parse(%q) = %v, want an *Error", f, err)
		}
	}
}

func TestOrder(t *testing.T) {
	for _, c := range []struct{ orderBy, want string }{
		{"", "web-1 web-2 builder db"},
		{"name", "builder db web-1 web-2"},
		{"name asc", "builder db web-1 web-2"},
		{"creationTimestamp desc", "web-2 db web-1 builder"},
		{"id", "builder db web-2 web-1"}, // int64s as strings, numerically
		{"labels.env, name desc", "builder web-2 web-1 db"},
		{"cpus desc,name", "db builder web-1 web-2"},
	} {
		o, err := ParseOrder(c.orderBy)
		if err != nil {
			t.Fatalf("ParseOrder(%q): %v", c.orderBy, err)
		}
		l := append([]map[string]any(nil), objects...)
		sort.SliceStable(l, func(i, j int) bool { return o.Less(l[i], l[j]) })
		var got []string
		for _, x := range l {
			got = append(got, x["name"].(string))
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("orderBy %q: %v, want %s", c.orderBy, got, c.want)
		}
	}
	for _, s := range []string{"name sideways", "name,", "a b c", "name()", "labels..env"} {
		if _, err := ParseOrder(s); err == nil {
			t.Errorf("ParseOrder(%q) parsed", s)
		}
	}
}
