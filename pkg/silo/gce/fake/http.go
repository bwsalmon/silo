package fake

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// Server is a Fake served over HTTP: compute/v1 under /compute/v1/ and
// Cloud Quotas under /v1/, in GCE's REST shapes, so that a real client
// can be pointed at it.
type Server struct {
	*httptest.Server
	Fake *Fake
}

// NewServer serves f on a local port. Close it when done.
func NewServer(f *Fake) *Server {
	return &Server{Server: httptest.NewServer(f.Handler()), Fake: f}
}

// ComputeEndpoint is the compute/v1 base URL, for
// option.WithEndpoint: http://127.0.0.1:<port>/compute/v1/.
func (s *Server) ComputeEndpoint() string { return s.URL + "/compute/v1/" }

// CloudQuotasEndpoint is the Cloud Quotas base URL, under which
// v1/projects/<p>/locations/global/services/compute.googleapis.com/quotaInfos
// is served.
func (s *Server) CloudQuotasEndpoint() string { return s.URL + "/" }

type route func(r *http.Request) (any, error)

// Handler serves f in compute/v1's REST shape.
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn route) {
		method, path, _ := cut(pattern)
		mux.HandleFunc(method+" /compute/v1/projects/{project}"+path, func(w http.ResponseWriter, r *http.Request) {
			v, err := fn(r)
			reply(w, v, err)
		})
	}
	pv := func(r *http.Request, k string) string { return r.PathValue(k) }
	q := func(r *http.Request, k string) string { return r.URL.Query().Get(k) }
	p := func(r *http.Request) string { return r.PathValue("project") }
	z := func(r *http.Request) string { return r.PathValue("zone") }
	rg := func(r *http.Request) string { return r.PathValue("region") }
	n := func(r *http.Request) string { return r.PathValue("name") }
	ctx := func(r *http.Request) context.Context { return r.Context() }

	// projects, regions, zones and the catalog.
	h("GET ", func(r *http.Request) (any, error) { return f.GetProject(ctx(r), p(r)) })
	h("GET /regions", func(r *http.Request) (any, error) {
		return listed(f.ListRegions(ctx(r), p(r)))(r, "compute#regionList")
	})
	h("GET /regions/{region}", func(r *http.Request) (any, error) { return f.GetRegion(ctx(r), p(r), rg(r)) })
	h("GET /zones", func(r *http.Request) (any, error) {
		return listed(f.ListZones(ctx(r), p(r)))(r, "compute#zoneList")
	})
	h("GET /zones/{zone}", func(r *http.Request) (any, error) { return f.GetZone(ctx(r), p(r), z(r)) })
	h("GET /zones/{zone}/machineTypes", func(r *http.Request) (any, error) {
		return listed(f.ListMachineTypes(ctx(r), p(r), z(r)))(r, "compute#machineTypeList")
	})
	h("GET /zones/{zone}/machineTypes/{name}", func(r *http.Request) (any, error) { return f.GetMachineType(ctx(r), p(r), z(r), n(r)) })
	h("GET /zones/{zone}/diskTypes", func(r *http.Request) (any, error) {
		return listed(f.ListDiskTypes(ctx(r), p(r), z(r)))(r, "compute#diskTypeList")
	})
	h("GET /zones/{zone}/diskTypes/{name}", func(r *http.Request) (any, error) { return f.GetDiskType(ctx(r), p(r), z(r), n(r)) })
	h("GET /zones/{zone}/acceleratorTypes", func(r *http.Request) (any, error) {
		return listed(f.ListAcceleratorTypes(ctx(r), p(r), z(r)))(r, "compute#acceleratorTypeList")
	})
	h("GET /zones/{zone}/acceleratorTypes/{name}", func(r *http.Request) (any, error) { return f.GetAcceleratorType(ctx(r), p(r), z(r), n(r)) })

	// operations.
	for _, sc := range []struct {
		prefix string
		scope  func(r *http.Request) gce.Scope
		kind   string
	}{
		{"/zones/{zone}/operations", func(r *http.Request) gce.Scope { return gce.Zonal(z(r)) }, "compute#operationList"},
		{"/regions/{region}/operations", func(r *http.Request) gce.Scope { return gce.Regional(rg(r)) }, "compute#operationList"},
		{"/global/operations", func(*http.Request) gce.Scope { return gce.Global }, "compute#operationList"},
	} {
		sc := sc
		h("GET "+sc.prefix, func(r *http.Request) (any, error) {
			return listed(f.ListOperations(ctx(r), p(r), sc.scope(r)))(r, sc.kind)
		})
		h("GET "+sc.prefix+"/{name}", func(r *http.Request) (any, error) { return f.GetOperation(ctx(r), p(r), sc.scope(r), n(r)) })
		h("POST "+sc.prefix+"/{name}/wait", func(r *http.Request) (any, error) { return f.WaitOperation(ctx(r), p(r), sc.scope(r), n(r)) })
	}

	// instances.
	h("GET /zones/{zone}/instances", func(r *http.Request) (any, error) {
		lf, err := filterOf(r)
		if err != nil {
			return nil, err
		}
		return listed(f.ListInstances(ctx(r), p(r), z(r), lf))(r, "compute#instanceList")
	})
	h("POST /zones/{zone}/instances", func(r *http.Request) (any, error) {
		body, err := decode[compute.Instance](r)
		if err != nil {
			return nil, err
		}
		return f.InsertInstance(ctx(r), p(r), z(r), body, q(r, "sourceInstanceTemplate"))
	})
	h("GET /zones/{zone}/instances/{name}", func(r *http.Request) (any, error) { return f.GetInstance(ctx(r), p(r), z(r), n(r)) })
	h("DELETE /zones/{zone}/instances/{name}", func(r *http.Request) (any, error) { return f.DeleteInstance(ctx(r), p(r), z(r), n(r)) })
	h("POST /zones/{zone}/instances/{name}/start", func(r *http.Request) (any, error) { return f.StartInstance(ctx(r), p(r), z(r), n(r)) })
	h("POST /zones/{zone}/instances/{name}/stop", func(r *http.Request) (any, error) {
		return f.StopInstance(ctx(r), p(r), z(r), n(r), q(r, "discardLocalSsd") == "true")
	})
	h("POST /zones/{zone}/instances/{name}/reset", func(r *http.Request) (any, error) { return f.ResetInstance(ctx(r), p(r), z(r), n(r)) })
	h("POST /zones/{zone}/instances/{name}/suspend", func(r *http.Request) (any, error) { return f.SuspendInstance(ctx(r), p(r), z(r), n(r)) })
	h("POST /zones/{zone}/instances/{name}/resume", func(r *http.Request) (any, error) { return f.ResumeInstance(ctx(r), p(r), z(r), n(r)) })
	h("POST /zones/{zone}/instances/{name}/attachDisk", func(r *http.Request) (any, error) {
		body, err := decode[compute.AttachedDisk](r)
		if err != nil {
			return nil, err
		}
		return f.AttachDisk(ctx(r), p(r), z(r), n(r), body)
	})
	h("POST /zones/{zone}/instances/{name}/detachDisk", func(r *http.Request) (any, error) {
		return f.DetachDisk(ctx(r), p(r), z(r), n(r), q(r, "deviceName"))
	})
	h("POST /zones/{zone}/instances/{name}/setLabels", func(r *http.Request) (any, error) {
		body, err := decode[compute.InstancesSetLabelsRequest](r)
		if err != nil {
			return nil, err
		}
		return f.SetInstanceLabels(ctx(r), p(r), z(r), n(r), body)
	})
	h("POST /zones/{zone}/instances/{name}/setMetadata", func(r *http.Request) (any, error) {
		body, err := decode[compute.Metadata](r)
		if err != nil {
			return nil, err
		}
		return f.SetInstanceMetadata(ctx(r), p(r), z(r), n(r), body)
	})
	h("POST /zones/{zone}/instances/{name}/setTags", func(r *http.Request) (any, error) {
		body, err := decode[compute.Tags](r)
		if err != nil {
			return nil, err
		}
		return f.SetInstanceTags(ctx(r), p(r), z(r), n(r), body)
	})
	h("POST /zones/{zone}/instances/{name}/setMachineType", func(r *http.Request) (any, error) {
		body, err := decode[compute.InstancesSetMachineTypeRequest](r)
		if err != nil {
			return nil, err
		}
		return f.SetMachineType(ctx(r), p(r), z(r), n(r), body.MachineType)
	})
	h("POST /zones/{zone}/instances/{name}/setDeletionProtection", func(r *http.Request) (any, error) {
		return f.SetDeletionProtection(ctx(r), p(r), z(r), n(r), q(r, "deletionProtection") != "false")
	})
	h("GET /zones/{zone}/instances/{name}/serialPort", func(r *http.Request) (any, error) {
		port, _ := strconv.ParseInt(q(r, "port"), 10, 64)
		start, _ := strconv.ParseInt(q(r, "start"), 10, 64)
		return f.GetSerialPortOutput(ctx(r), p(r), z(r), n(r), port, start)
	})

	// disks.
	h("GET /zones/{zone}/disks", func(r *http.Request) (any, error) {
		lf, err := filterOf(r)
		if err != nil {
			return nil, err
		}
		return listed(f.ListDisks(ctx(r), p(r), z(r), lf))(r, "compute#diskList")
	})
	h("POST /zones/{zone}/disks", func(r *http.Request) (any, error) {
		body, err := decode[compute.Disk](r)
		if err != nil {
			return nil, err
		}
		return f.InsertDisk(ctx(r), p(r), z(r), body)
	})
	h("GET /zones/{zone}/disks/{name}", func(r *http.Request) (any, error) { return f.GetDisk(ctx(r), p(r), z(r), n(r)) })
	h("DELETE /zones/{zone}/disks/{name}", func(r *http.Request) (any, error) { return f.DeleteDisk(ctx(r), p(r), z(r), n(r)) })
	h("PATCH /zones/{zone}/disks/{name}", func(r *http.Request) (any, error) {
		body, err := decode[compute.Disk](r)
		if err != nil {
			return nil, err
		}
		return f.UpdateDisk(ctx(r), p(r), z(r), n(r), body, r.URL.Query()["paths"])
	})
	h("POST /zones/{zone}/disks/{name}/resize", func(r *http.Request) (any, error) {
		body, err := decode[compute.DisksResizeRequest](r)
		if err != nil {
			return nil, err
		}
		return f.ResizeDisk(ctx(r), p(r), z(r), n(r), body.SizeGb)
	})
	h("POST /zones/{zone}/disks/{name}/setLabels", func(r *http.Request) (any, error) {
		body, err := decode[compute.ZoneSetLabelsRequest](r)
		if err != nil {
			return nil, err
		}
		return f.SetDiskLabels(ctx(r), p(r), z(r), n(r), body)
	})
	h("POST /zones/{zone}/disks/{name}/createSnapshot", func(r *http.Request) (any, error) {
		body, err := decode[compute.Snapshot](r)
		if err != nil {
			return nil, err
		}
		return f.CreateSnapshot(ctx(r), p(r), z(r), n(r), body)
	})

	// images.
	h("GET /global/images", func(r *http.Request) (any, error) {
		lf, err := filterOf(r)
		if err != nil {
			return nil, err
		}
		return listed(f.ListImages(ctx(r), p(r), lf))(r, "compute#imageList")
	})
	h("POST /global/images", func(r *http.Request) (any, error) {
		body, err := decode[compute.Image](r)
		if err != nil {
			return nil, err
		}
		return f.InsertImage(ctx(r), p(r), body)
	})
	h("GET /global/images/{name}", func(r *http.Request) (any, error) { return f.GetImage(ctx(r), p(r), n(r)) })
	h("GET /global/images/family/{family}", func(r *http.Request) (any, error) {
		return f.GetImageFromFamily(ctx(r), p(r), pv(r, "family"))
	})
	h("DELETE /global/images/{name}", func(r *http.Request) (any, error) { return f.DeleteImage(ctx(r), p(r), n(r)) })
	h("POST /global/images/{name}/setLabels", func(r *http.Request) (any, error) {
		body, err := decode[compute.GlobalSetLabelsRequest](r)
		if err != nil {
			return nil, err
		}
		return f.SetImageLabels(ctx(r), p(r), n(r), body)
	})
	h("POST /global/images/{name}/deprecate", func(r *http.Request) (any, error) {
		body, err := decode[compute.DeprecationStatus](r)
		if err != nil {
			return nil, err
		}
		return f.DeprecateImage(ctx(r), p(r), n(r), body)
	})

	// snapshots.
	h("GET /global/snapshots", func(r *http.Request) (any, error) {
		lf, err := filterOf(r)
		if err != nil {
			return nil, err
		}
		return listed(f.ListSnapshots(ctx(r), p(r), lf))(r, "compute#snapshotList")
	})
	h("GET /global/snapshots/{name}", func(r *http.Request) (any, error) { return f.GetSnapshot(ctx(r), p(r), n(r)) })
	h("DELETE /global/snapshots/{name}", func(r *http.Request) (any, error) { return f.DeleteSnapshot(ctx(r), p(r), n(r)) })
	h("POST /global/snapshots/{name}/setLabels", func(r *http.Request) (any, error) {
		body, err := decode[compute.GlobalSetLabelsRequest](r)
		if err != nil {
			return nil, err
		}
		return f.SetSnapshotLabels(ctx(r), p(r), n(r), body)
	})

	// instance templates, global and regional.
	for _, sc := range []struct {
		prefix string
		region func(r *http.Request) string
	}{
		{"/global/instanceTemplates", func(*http.Request) string { return "" }},
		{"/regions/{region}/instanceTemplates", rg},
	} {
		sc := sc
		h("GET "+sc.prefix, func(r *http.Request) (any, error) {
			lf, err := filterOf(r)
			if err != nil {
				return nil, err
			}
			return listed(f.ListInstanceTemplates(ctx(r), p(r), sc.region(r), lf))(r, "compute#instanceTemplateList")
		})
		h("POST "+sc.prefix, func(r *http.Request) (any, error) {
			body, err := decode[compute.InstanceTemplate](r)
			if err != nil {
				return nil, err
			}
			return f.InsertInstanceTemplate(ctx(r), p(r), sc.region(r), body)
		})
		h("GET "+sc.prefix+"/{name}", func(r *http.Request) (any, error) {
			return f.GetInstanceTemplate(ctx(r), p(r), sc.region(r), n(r))
		})
		h("DELETE "+sc.prefix+"/{name}", func(r *http.Request) (any, error) {
			return f.DeleteInstanceTemplate(ctx(r), p(r), sc.region(r), n(r))
		})
	}

	// networks.
	h("GET /global/networks", func(r *http.Request) (any, error) {
		lf, err := filterOf(r)
		if err != nil {
			return nil, err
		}
		return listed(f.ListNetworks(ctx(r), p(r), lf))(r, "compute#networkList")
	})
	h("POST /global/networks", func(r *http.Request) (any, error) {
		body, err := decode[compute.Network](r)
		if err != nil {
			return nil, err
		}
		return f.InsertNetwork(ctx(r), p(r), body)
	})
	h("GET /global/networks/{name}", func(r *http.Request) (any, error) { return f.GetNetwork(ctx(r), p(r), n(r)) })
	h("DELETE /global/networks/{name}", func(r *http.Request) (any, error) { return f.DeleteNetwork(ctx(r), p(r), n(r)) })

	// subnetworks.
	h("GET /regions/{region}/subnetworks", func(r *http.Request) (any, error) {
		lf, err := filterOf(r)
		if err != nil {
			return nil, err
		}
		return listed(f.ListSubnetworks(ctx(r), p(r), rg(r), lf))(r, "compute#subnetworkList")
	})
	h("POST /regions/{region}/subnetworks", func(r *http.Request) (any, error) {
		body, err := decode[compute.Subnetwork](r)
		if err != nil {
			return nil, err
		}
		return f.InsertSubnetwork(ctx(r), p(r), rg(r), body)
	})
	h("GET /regions/{region}/subnetworks/{name}", func(r *http.Request) (any, error) { return f.GetSubnetwork(ctx(r), p(r), rg(r), n(r)) })
	h("DELETE /regions/{region}/subnetworks/{name}", func(r *http.Request) (any, error) {
		return f.DeleteSubnetwork(ctx(r), p(r), rg(r), n(r))
	})
	h("PATCH /regions/{region}/subnetworks/{name}", func(r *http.Request) (any, error) {
		body, err := decode[compute.Subnetwork](r)
		if err != nil {
			return nil, err
		}
		return f.PatchSubnetwork(ctx(r), p(r), rg(r), n(r), body)
	})
	h("POST /regions/{region}/subnetworks/{name}/expandIpCidrRange", func(r *http.Request) (any, error) {
		body, err := decode[compute.SubnetworksExpandIpCidrRangeRequest](r)
		if err != nil {
			return nil, err
		}
		return f.ExpandIpCidrRange(ctx(r), p(r), rg(r), n(r), body.IpCidrRange)
	})

	// firewalls.
	h("GET /global/firewalls", func(r *http.Request) (any, error) {
		lf, err := filterOf(r)
		if err != nil {
			return nil, err
		}
		return listed(f.ListFirewalls(ctx(r), p(r), lf))(r, "compute#firewallList")
	})
	h("POST /global/firewalls", func(r *http.Request) (any, error) {
		body, err := decode[compute.Firewall](r)
		if err != nil {
			return nil, err
		}
		return f.InsertFirewall(ctx(r), p(r), body)
	})
	h("GET /global/firewalls/{name}", func(r *http.Request) (any, error) { return f.GetFirewall(ctx(r), p(r), n(r)) })
	h("DELETE /global/firewalls/{name}", func(r *http.Request) (any, error) { return f.DeleteFirewall(ctx(r), p(r), n(r)) })
	h("PATCH /global/firewalls/{name}", func(r *http.Request) (any, error) {
		body, err := decode[compute.Firewall](r)
		if err != nil {
			return nil, err
		}
		return f.PatchFirewall(ctx(r), p(r), n(r), body)
	})
	h("PUT /global/firewalls/{name}", func(r *http.Request) (any, error) {
		body, err := decode[compute.Firewall](r)
		if err != nil {
			return nil, err
		}
		return f.UpdateFirewall(ctx(r), p(r), n(r), body)
	})

	// network firewall policies.
	prio := func(r *http.Request) (int64, error) {
		v, err := strconv.ParseInt(q(r, "priority"), 10, 64)
		if err != nil {
			return 0, invalid("priority", "'%s'", q(r, "priority"))
		}
		return v, nil
	}
	h("GET /global/firewallPolicies", func(r *http.Request) (any, error) {
		return listed(f.ListFirewallPolicies(ctx(r), p(r)))(r, "compute#firewallPolicyList")
	})
	h("POST /global/firewallPolicies", func(r *http.Request) (any, error) {
		body, err := decode[compute.FirewallPolicy](r)
		if err != nil {
			return nil, err
		}
		return f.InsertFirewallPolicy(ctx(r), p(r), body)
	})
	h("GET /global/firewallPolicies/{name}", func(r *http.Request) (any, error) { return f.GetFirewallPolicy(ctx(r), p(r), n(r)) })
	h("DELETE /global/firewallPolicies/{name}", func(r *http.Request) (any, error) {
		return f.DeleteFirewallPolicy(ctx(r), p(r), n(r))
	})
	h("PATCH /global/firewallPolicies/{name}", func(r *http.Request) (any, error) {
		body, err := decode[compute.FirewallPolicy](r)
		if err != nil {
			return nil, err
		}
		return f.PatchFirewallPolicy(ctx(r), p(r), n(r), body)
	})
	h("POST /global/firewallPolicies/{name}/addRule", func(r *http.Request) (any, error) {
		body, err := decode[compute.FirewallPolicyRule](r)
		if err != nil {
			return nil, err
		}
		return f.AddFirewallPolicyRule(ctx(r), p(r), n(r), body)
	})
	h("GET /global/firewallPolicies/{name}/getRule", func(r *http.Request) (any, error) {
		pr, err := prio(r)
		if err != nil {
			return nil, err
		}
		return f.GetFirewallPolicyRule(ctx(r), p(r), n(r), pr)
	})
	h("POST /global/firewallPolicies/{name}/patchRule", func(r *http.Request) (any, error) {
		pr, err := prio(r)
		if err != nil {
			return nil, err
		}
		body, err := decode[compute.FirewallPolicyRule](r)
		if err != nil {
			return nil, err
		}
		return f.PatchFirewallPolicyRule(ctx(r), p(r), n(r), pr, body)
	})
	h("POST /global/firewallPolicies/{name}/removeRule", func(r *http.Request) (any, error) {
		pr, err := prio(r)
		if err != nil {
			return nil, err
		}
		return f.RemoveFirewallPolicyRule(ctx(r), p(r), n(r), pr)
	})
	h("POST /global/firewallPolicies/{name}/addAssociation", func(r *http.Request) (any, error) {
		body, err := decode[compute.FirewallPolicyAssociation](r)
		if err != nil {
			return nil, err
		}
		return f.AddFirewallPolicyAssociation(ctx(r), p(r), n(r), body, q(r, "replaceExistingAssociation") == "true")
	})
	h("POST /global/firewallPolicies/{name}/removeAssociation", func(r *http.Request) (any, error) {
		return f.RemoveFirewallPolicyAssociation(ctx(r), p(r), n(r), q(r, "name"))
	})

	// Cloud Quotas.
	mux.HandleFunc("GET /v1/projects/{project}/locations/global/services/{service}/quotaInfos", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("service") != gce.CloudQuotasService {
			reply(w, nil, gce.NotFound("services/"+r.PathValue("service")))
			return
		}
		infos, err := f.ListComputeQuotaInfos(r.Context(), p(r))
		if err != nil {
			reply(w, nil, err)
			return
		}
		items, next, err := page(r, infos)
		reply(w, map[string]any{"quotaInfos": items, "nextPageToken": next}, err)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, gce.NewError(http.StatusNotFound, gce.ReasonNotFound, "the fake doesn't serve %s %s", r.Method, r.URL.Path))
	})
	return mux
}

func cut(pattern string) (method, path string, ok bool) {
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == ' ' {
			return pattern[:i], pattern[i+1:], true
		}
	}
	return pattern, "", false
}

// listed turns a List call's result into a function of the request
// that pages it and wraps it in GCE's list shape.
func listed[T any](items []*T, err error) func(r *http.Request, kind string) (any, error) {
	return func(r *http.Request, kind string) (any, error) {
		if err != nil {
			return nil, err
		}
		pg, next, err := page(r, items)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"kind": kind, "id": r.URL.Path, "selfLink": gce.SelfLinkBase + trimToV1(r.URL.Path)}
		if len(pg) > 0 {
			out["items"] = pg
		}
		if next != "" {
			out["nextPageToken"] = next
		}
		return out, nil
	}
}

func trimToV1(path string) string {
	const prefix = "/compute/v1/"
	if len(path) > len(prefix) && path[:len(prefix)] == prefix {
		return path[len(prefix):]
	}
	return path
}

// page applies maxResults and pageToken, which is the offset.
func page[T any](r *http.Request, items []T) ([]T, string, error) {
	start := 0
	if tok := r.URL.Query().Get("pageToken"); tok != "" {
		v, err := strconv.Atoi(tok)
		if err != nil || v < 0 || v > len(items) {
			return nil, "", invalid("pageToken", "'%s'", tok)
		}
		start = v
	}
	end := len(items)
	if m := r.URL.Query().Get("maxResults"); m != "" {
		v, err := strconv.Atoi(m)
		if err != nil || v < 0 {
			return nil, "", invalid("maxResults", "'%s'", m)
		}
		if v > 0 && start+v < end {
			end = start + v
		}
	}
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next, nil
}

func filterOf(r *http.Request) (gce.ListFilter, error) {
	lf, err := gce.ParseListFilter(r.URL.Query().Get("filter"))
	if err != nil {
		return gce.ListFilter{}, invalid("filter", "%v", err)
	}
	return lf, nil
}

func decode[T any](r *http.Request) (*T, error) {
	v := new(T)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return nil, gce.NewError(http.StatusBadRequest, "parseError", "Parse Error: %v", err)
	}
	return v, nil
}

// reply writes v as JSON, or err in GCE's error shape.
func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if err != nil {
		var ge *googleapi.Error
		if !errors.As(err, &ge) {
			ge = gce.NewError(http.StatusInternalServerError, gce.ReasonBackendError, "%v", err)
		}
		items := []map[string]string{}
		for _, it := range ge.Errors {
			items = append(items, map[string]string{"domain": "global", "reason": it.Reason, "message": it.Message})
		}
		w.WriteHeader(ge.Code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": ge.Code, "message": ge.Message, "errors": items,
		}})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
