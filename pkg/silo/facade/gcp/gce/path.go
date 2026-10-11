package gce

import (
	"net/http"
	"strings"
	"unicode"
)

// call is a compute/v1 path, parsed: which resource and method it
// names, in which project and scope.
type call struct {
	Project  string
	Resource string // GCE's resource name: instances, zoneOperations, regionInstanceTemplates
	Method   string // GCE's method name: get, list, insert, setLabels, wait
	Zone     string
	Region   string
	Global   bool
	// Collection is the path's own collection segment (operations,
	// instanceTemplates), which Resource refines by scope.
	Collection string
	// Name is the item the path names, as the tenant wrote it: an
	// instance, an operation, an image family for getFromFamily.
	Name string
}

// FullMethod is resource.method.
func (c call) FullMethod() string { return c.Resource + "." + c.Method }

// Scope is the call's scope as a partial path: zones/z, regions/r or
// global.
func (c call) Scope() string {
	switch {
	case c.Zone != "":
		return "zones/" + c.Zone
	case c.Region != "":
		return "regions/" + c.Region
	}
	return "global"
}

// regionalNames are GCE's resources whose regional collection has the
// same name as the resource. Any other regional collection is
// region<Collection> (regionDisks, regionInstanceTemplates).
var regionalNames = map[string]bool{
	"subnetworks": true, "addresses": true, "routers": true, "forwardingRules": true,
	"vpnGateways": true, "vpnTunnels": true, "targetVpnGateways": true, "interconnectAttachments": true,
	"serviceAttachments": true, "targetPools": true, "nodeTemplates": true, "resourcePolicies": true,
	"packetMirrorings": true, "networkAttachments": true, "publicDelegatedPrefixes": true,
}

// globalNames are the global collections whose resource isn't named
// after them.
var globalNames = map[string]string{
	"operations": "globalOperations", "addresses": "globalAddresses",
	"forwardingRules": "globalForwardingRules", "firewallPolicies": "networkFirewallPolicies",
	"publicDelegatedPrefixes": "globalPublicDelegatedPrefixes",
}

// Method names the facade method a request for path (/compute/v1/...)
// with HTTP method verb calls, as "resource.method" (instances.insert),
// for control.FacadeClass. It is "" for a path that names none, which
// that classifies as a mutation. The gateway classifies with it, so
// that it and the facade parse a path the same way.
func Method(verb, path string) string {
	rest, ok := strings.CutPrefix(path, "/compute/v1/")
	if !ok {
		return ""
	}
	c, ok := parsePath(verb, rest)
	if !ok || c.Resource == "" || c.Method == "" {
		return ""
	}
	return c.FullMethod()
}

// parsePath parses rest, the path after /compute/v1/, for HTTP method
// verb. ok is false for a path that isn't under projects/<project>.
// A path that names no method GCE has leaves Method empty.
func parsePath(verb, rest string) (c call, ok bool) {
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segs) < 2 || segs[0] != "projects" || segs[1] == "" {
		return c, false
	}
	c.Project = segs[1]
	segs = segs[2:]
	if len(segs) == 0 {
		c.Resource = "projects"
		if verb == http.MethodGet {
			c.Method = "get"
		}
		return c, true
	}
	switch segs[0] {
	case "aggregated":
		if len(segs) == 2 {
			c.Resource, c.Collection = segs[1], segs[1]
			if verb == http.MethodGet {
				c.Method = "aggregatedList"
			}
			return c, true
		}
		c.Resource = "aggregated"
		return c, true
	case "zones", "regions":
		res := segs[0]
		if len(segs) == 1 {
			c.Resource = res
			if verb == http.MethodGet {
				c.Method = "list"
			}
			return c, true
		}
		if res == "zones" {
			c.Zone = segs[1]
		} else {
			c.Region = segs[1]
		}
		if len(segs) == 2 {
			c.Resource = res
			if verb == http.MethodGet {
				c.Method = "get"
			}
			c.Name = segs[1]
			return c, true
		}
		segs = segs[2:]
	case "global":
		c.Global = true
		segs = segs[1:]
		if len(segs) == 0 {
			c.Resource = "global"
			return c, true
		}
	default:
		// projects.<verb>: setCommonInstanceMetadata and the rest.
		c.Resource = "projects"
		if len(segs) == 1 && verb == http.MethodPost {
			c.Method = segs[0]
		}
		return c, true
	}
	c.Collection = segs[0]
	c.Resource = resourceOf(c)
	item := segs[1:]
	switch {
	case len(item) == 0:
		c.Method = map[string]string{http.MethodGet: "list", http.MethodPost: "insert"}[verb]
	case len(item) == 1:
		c.Name = item[0]
		c.Method = map[string]string{http.MethodGet: "get", http.MethodDelete: "delete",
			http.MethodPatch: "patch", http.MethodPut: "update", http.MethodPost: ""}[verb]
		if verb == http.MethodPatch && c.Collection == "disks" {
			// GCE's disks.update is a PATCH; disks have no patch.
			c.Method = "update"
		}
		if verb == http.MethodPost && c.Collection != "operations" {
			// A collection-level custom method: instances/bulkInsert.
			c.Method, c.Name = item[0], ""
		}
	case len(item) == 2 && c.Collection == "images" && item[0] == "family":
		c.Name = item[1]
		if verb == http.MethodGet {
			c.Method = "getFromFamily"
		}
	case len(item) == 2:
		c.Name = item[0]
		switch {
		case item[1] == "serialPort" && verb == http.MethodGet:
			c.Method = "getSerialPortOutput"
		case verb == http.MethodPost || verb == http.MethodGet:
			c.Method = item[1]
		}
	}
	return c, true
}

func resourceOf(c call) string {
	switch {
	case c.Zone != "":
		if c.Collection == "operations" {
			return "zoneOperations"
		}
		return c.Collection
	case c.Region != "":
		if regionalNames[c.Collection] {
			return c.Collection
		}
		return "region" + upperFirst(c.Collection)
	}
	if r, ok := globalNames[c.Collection]; ok {
		return r
	}
	return c.Collection
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
