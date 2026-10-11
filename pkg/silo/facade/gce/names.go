package gce

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxTenantName is the longest name a tenant may give an object: the
// real name, <id>-<name>, must fit GCE's 63 (gcp-backend.md, "Naming",
// SV-76).
const MaxTenantName = 56

// tenantCollections are the collections whose objects the tenant
// names, so whose real names carry the <id>- prefix.
var tenantCollections = map[string]bool{
	"instances": true, "disks": true, "images": true, "snapshots": true, "firewalls": true,
	"subnetworks": true, "instanceTemplates": true,
}

// facadeNetwork is the one network the facade shows (call 12).
const facadeNetwork = "default"

// linkHosts are the URL prefixes a request may name an object by, up
// to /compute/v1/.
var linkHost = regexp.MustCompile(`^https?://(silo\.internal(:[0-9]+)?|www\.googleapis\.com|compute\.googleapis\.com)/compute/v1/`)

// namer maps names between a silo's facade and the host project.
type namer struct {
	silo    string // the silo whose facade it is
	host    string // the host project
	base    string // the facade's base URL, ending /compute/v1/
	network string // the shared VPC's real name
	// public are the public image projects, whose names pass through.
	public map[string]bool
}

func (n *namer) project() string { return "silo-" + n.silo }

func (n *namer) prefix() string { return n.silo + "-" }

// realName is the host project's name for the tenant's name.
func (n *namer) realName(name string) string { return n.prefix() + name }

// tenantName is the tenant's name for a real name, and whether the real
// name is the silo's at all.
func (n *namer) tenantName(real string) (string, bool) {
	return strings.CutPrefix(real, n.prefix())
}

var tenantNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// checkName refuses a tenant's name its real name couldn't carry.
func checkName(name string) error {
	if len(name) > MaxTenantName {
		return invalid("Invalid value for field 'name': '%s'. silo: names are at most %d characters, because the real name carries the silo's prefix", name, MaxTenantName)
	}
	if !tenantNameRE.MatchString(name) {
		return invalid("Invalid value for field 'name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", name)
	}
	return nil
}

// mapPath rewrites a path's segments from projects/ (or zones/,
// regions/, global/) on, with f deciding each project and each item.
func mapPath(segs []string, project func(string) (string, bool), item func(coll, name string) (string, bool)) ([]string, bool) {
	out := make([]string, len(segs))
	copy(out, segs)
	for i := 0; i+1 < len(out); i++ {
		switch out[i] {
		case "projects":
			p, ok := project(out[i+1])
			if !ok {
				return nil, false
			}
			out[i+1] = p
			i++
		case "zones", "regions":
			i++
		case "global", "aggregated":
		default:
			if out[i] == "images" && out[i+1] == "family" {
				i += 2 // images/family/<family>
				continue
			}
			name, ok := item(out[i], out[i+1])
			if !ok {
				return nil, false
			}
			out[i+1] = name
			i++
		}
	}
	return out, true
}

// splitLink splits a full URL or a partial path into the part before
// the path and the path's segments. A string that is neither is
// returned with ok false.
func splitLink(s string) (head string, segs []string, ok bool) {
	if m := linkHost.FindString(s); m != "" {
		return m, strings.Split(strings.TrimSuffix(s[len(m):], "/"), "/"), true
	}
	for _, p := range []string{"projects/", "zones/", "regions/", "global/"} {
		if strings.HasPrefix(s, p) {
			return "", strings.Split(strings.TrimSuffix(s, "/"), "/"), true
		}
	}
	return "", nil, false
}

// outLink rewrites a link GCE wrote into the facade's: the host
// project is the silo's, tenant objects lose their prefix, the shared
// VPC is default, and a full URL is on the facade. A link that isn't
// one is returned unchanged.
func (n *namer) outLink(s string) string {
	head, segs, ok := splitLink(s)
	if !ok {
		return s
	}
	out, _ := mapPath(segs,
		func(p string) (string, bool) {
			if p == n.host {
				return n.project(), true
			}
			return p, true
		},
		func(coll, name string) (string, bool) {
			if coll == "networks" && name == n.network {
				return facadeNetwork, true
			}
			if tenantCollections[coll] {
				if t, ok := n.tenantName(name); ok {
					return t, true
				}
			}
			return name, true
		})
	path := strings.Join(out, "/")
	if head == "" {
		return path
	}
	return n.base + path
}

// inLink maps a link a tenant wrote, bare, partial or a full URL on
// silo.internal, the facade's own endpoint, www.googleapis.com or
// compute.googleapis.com, into the
// host project's. coll is the collection a bare name is in. A link to
// another project is refused, except a public image project's, which
// passes through; one naming a tenant object by a name too long is
// 400 invalid.
func (n *namer) inLink(s, coll string) (string, error) {
	if n.base != "" && !linkHost.MatchString(s) && strings.HasPrefix(s, n.base) {
		// The facade's own selfLinks, on whatever endpoint it serves:
		// what it wrote out, a template's properties included, comes
		// back in.
		s = "https://www.googleapis.com/compute/v1/" + s[len(n.base):]
	}
	head, segs, ok := splitLink(s)
	if !ok {
		if coll == "networks" {
			if s != facadeNetwork {
				return "", notFound("projects/" + n.project() + "/global/networks/" + s)
			}
			return n.network, nil
		}
		if !tenantCollections[coll] {
			return s, nil
		}
		if err := checkName(s); err != nil {
			return "", err
		}
		return n.realName(s), nil
	}
	var bad error
	out, ok := mapPath(segs,
		func(p string) (string, bool) {
			switch {
			case p == n.project() || p == n.host:
				return n.host, true
			case n.public[p]:
				return p, true
			}
			bad = forbidden("silo: %s names project %s, which this call can't reach", s, p)
			return "", false
		},
		func(c, name string) (string, bool) {
			switch {
			case c == "networks":
				if name != facadeNetwork {
					bad = notFound(s)
					return "", false
				}
				return n.network, true
			case tenantCollections[c]:
				if err := checkName(name); err != nil {
					bad = err
					return "", false
				}
				return n.realName(name), true
			}
			return name, true
		})
	if !ok {
		return "", bad
	}
	if len(segs) > 1 && segs[0] == "projects" && n.public[segs[1]] {
		// A public image keeps its own project and name.
		out = segs
	}
	path := strings.Join(out, "/")
	if head == "" {
		return path, nil
	}
	return "https://www.googleapis.com/compute/v1/" + path, nil
}

// isLink reports whether s is a URL or a partial path the facade
// rewrites.
func isLink(s string) bool {
	_, _, ok := splitLink(s)
	if ok {
		return true
	}
	return strings.Contains(s, "/compute/v1/projects/")
}

// gceLink is any compute/v1 URL GCE writes.
var gceLink = regexp.MustCompile(`^https?://[^/]+/compute/v1/`)

// out rewrites a decoded GCE object for the tenant, in place: names and
// links mapped back, and the facade's own labels and tags gone.
func (n *namer) out(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch {
			case k == "labels":
				if m, ok := val.(map[string]any); ok {
					for lk := range m {
						if reservedLabel(lk) {
							delete(m, lk)
						}
					}
				}
				continue
			case k == "tags":
				if m, ok := val.(map[string]any); ok {
					m["items"] = n.outTags(m["items"])
				}
				continue
			case k == "targetTags" || k == "sourceTags":
				x[k] = n.outTags(val)
				continue
			}
			x[k] = n.out(val, k)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = n.out(e, key)
		}
		return x
	case string:
		switch {
		case key == "name":
			if t, ok := n.tenantName(x); ok {
				return t
			}
			if x == n.network {
				return facadeNetwork
			}
			return x
		case gceLink.MatchString(x):
			if m := linkHost.FindString(x); m == "" {
				// A compute/v1 URL on a host the facade doesn't know.
				x = "https://www.googleapis.com/compute/v1/" + x[len(gceLink.FindString(x)):]
			}
			return n.outLink(x)
		case isLink(x):
			return n.outLink(x)
		}
		return n.outText(x)
	}
	return v
}

// outText rewrites free text GCE wrote, a message or a description:
// the host project becomes the silo's project, and real names the
// tenant's.
func (n *namer) outText(s string) string {
	if n.host != "" {
		s = strings.ReplaceAll(s, "projects/"+n.host+"/", "projects/"+n.project()+"/")
		s = strings.ReplaceAll(s, "'"+n.host+"'", "'"+n.project()+"'")
	}
	s = strings.ReplaceAll(s, "networks/"+n.network+"'", "networks/"+facadeNetwork+"'")
	return strings.ReplaceAll(s, n.prefix(), "")
}

// outTags drops the facade's own tag, the silo id, and the prefix the
// facade gave the tenant's.
func (n *namer) outTags(v any) any {
	l, ok := v.([]any)
	if !ok {
		return v
	}
	out := []any{}
	for _, e := range l {
		s, _ := e.(string)
		if s == n.silo {
			continue
		}
		if t, ok := n.tenantName(s); ok {
			out = append(out, t)
		} else if !reservedLabel(s) {
			out = append(out, s)
		}
	}
	return out
}

// reservedLabel reports whether a label key or a tag is the facade's
// own: anything starting with silo.
func reservedLabel(k string) bool { return strings.HasPrefix(k, "silo") }

// checkReserved refuses a request that sets a label or a tag starting
// with silo, which are the facade's own (gcp-backend.md, "Layout").
func checkReserved(v any, key string) error {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			val := x[k]
			switch k {
			case "labels":
				if m, ok := val.(map[string]any); ok {
					for _, lk := range sortedKeys(m) {
						if reservedLabel(lk) {
							return forbidden("silo: label %s is reserved: labels starting with silo are the facade's own", lk)
						}
					}
				}
				continue
			case "targetTags", "sourceTags":
				if err := checkTags(val); err != nil {
					return err
				}
				continue
			case "items":
				if key == "tags" {
					if err := checkTags(val); err != nil {
						return err
					}
					continue
				}
			}
			if err := checkReserved(val, k); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := checkReserved(e, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkTags(v any) error {
	l, _ := v.([]any)
	for _, e := range l {
		if s, _ := e.(string); reservedLabel(s) {
			return forbidden("silo: tag %s is reserved: tags starting with silo are the facade's own", s)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }
