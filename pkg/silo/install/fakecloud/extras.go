package fakecloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/install"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

func conflict(what string) error {
	return &googleapi.Error{Code: http.StatusConflict, Message: what + " already exists"}
}

func badRequest(msg string) error {
	return &googleapi.Error{Code: http.StatusBadRequest, Message: msg}
}

// copyPolicy deep-copies p, so that a caller's changes reach the fake
// only through a set.
func copyPolicy(p *install.Policy) *install.Policy {
	var out install.Policy
	b, _ := json.Marshal(p)
	_ = json.Unmarshal(b, &out)
	return &out
}

// setPolicy is setIamPolicy over *cur: a stale etag is a 409.
func (f *Fake) setPolicy(cur **install.Policy, p *install.Policy) error {
	have := *cur
	if have == nil {
		have = &install.Policy{Etag: "0"}
	}
	if p.Etag != have.Etag {
		return &googleapi.Error{Code: http.StatusConflict, Message: "There were concurrent policy changes"}
	}
	n, _ := strconv.Atoi(have.Etag)
	next := copyPolicy(p)
	next.Etag = strconv.Itoa(n + 1)
	*cur = next
	return nil
}

func getPolicy(p *install.Policy) *install.Policy {
	if p == nil {
		return &install.Policy{Version: 3, Etag: "0"}
	}
	return copyPolicy(p)
}

func (r resourceManager) GetIamPolicy(ctx context.Context, project string) (*install.Policy, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return getPolicy(r.f.ProjectPolicy), r.f.need("cloudresourcemanager.googleapis.com")
}

func (r resourceManager) SetIamPolicy(ctx context.Context, project string, p *install.Policy) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if err := r.f.need("cloudresourcemanager.googleapis.com"); err != nil {
		return err
	}
	return r.f.setPolicy(&r.f.ProjectPolicy, p)
}

func (a iamAPI) PatchServiceAccount(ctx context.Context, project string, sa *install.ServiceAccount) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	cur := a.f.ServiceAccounts[sa.Email]
	if cur == nil {
		return notFound("service account " + sa.Email)
	}
	cur.DisplayName, cur.Description = sa.DisplayName, sa.Description
	return nil
}

func (a iamAPI) EnableServiceAccount(ctx context.Context, project, email string) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	cur := a.f.ServiceAccounts[email]
	if cur == nil {
		return notFound("service account " + email)
	}
	cur.Disabled = false
	return nil
}

func (a iamAPI) DeleteServiceAccount(ctx context.Context, project, email string) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if a.f.ServiceAccounts[email] == nil {
		return notFound("service account " + email)
	}
	delete(a.f.ServiceAccounts, email)
	return nil
}

func copyRole(r *install.Role) *install.Role {
	cp := *r
	cp.Permissions = append([]string(nil), r.Permissions...)
	return &cp
}

func (a iamAPI) GetRole(ctx context.Context, project, id string) (*install.Role, error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if r := a.f.Roles[id]; r != nil {
		return copyRole(r), nil
	}
	return nil, notFound("role " + id)
}

// CreateRole fails for a deleted role too: its id can't be used again
// until it is purged.
func (a iamAPI) CreateRole(ctx context.Context, project, id string, r *install.Role) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if a.f.Roles[id] != nil {
		return conflict("role " + id)
	}
	a.f.Roles[id] = copyRole(r)
	return nil
}

func (a iamAPI) UpdateRole(ctx context.Context, project, id string, r *install.Role) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if a.f.Roles[id] == nil {
		return notFound("role " + id)
	}
	a.f.Roles[id] = copyRole(r)
	a.f.Roles[id].Deleted = false
	return nil
}

func (a iamAPI) DeleteRole(ctx context.Context, project, id string) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	r := a.f.Roles[id]
	if r == nil || r.Deleted {
		return notFound("role " + id)
	}
	r.Deleted = true
	return nil
}

func (d dnsAPI) PatchManagedZone(ctx context.Context, project string, z *install.ManagedZone) error {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	cur := d.f.Zones[z.Name]
	if cur == nil {
		return notFound("managed zone " + z.Name)
	}
	cur.Networks = append([]string(nil), z.Networks...)
	return nil
}

func (d dnsAPI) DeleteManagedZone(ctx context.Context, project, name string) error {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if d.f.Zones[name] == nil {
		return notFound("managed zone " + name)
	}
	for k, rs := range d.f.Records {
		if rs.zone == name {
			return badRequest(fmt.Sprintf("zone %s still holds %s", name, k))
		}
	}
	delete(d.f.Zones, name)
	return nil
}

func recordKey(zone, name, typ string) string { return zone + "/" + name + "/" + typ }

func (d dnsAPI) GetRecordSet(ctx context.Context, project, zone, name, typ string) (*install.RecordSet, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if d.f.Zones[zone] == nil {
		return nil, notFound("managed zone " + zone)
	}
	if rs := d.f.Records[recordKey(zone, name, typ)]; rs != nil {
		cp := rs.RecordSet
		cp.Rrdatas = append([]string(nil), rs.Rrdatas...)
		return &cp, nil
	}
	return nil, notFound("record set " + name)
}

func (d dnsAPI) CreateRecordSet(ctx context.Context, project, zone string, rs *install.RecordSet) error {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if d.f.Zones[zone] == nil {
		return notFound("managed zone " + zone)
	}
	k := recordKey(zone, rs.Name, rs.Type)
	if d.f.Records[k] != nil {
		return conflict("record set " + rs.Name)
	}
	cp := *rs
	cp.Rrdatas = append([]string(nil), rs.Rrdatas...)
	d.f.Records[k] = &Record{zone: zone, RecordSet: cp}
	return nil
}

func (d dnsAPI) PatchRecordSet(ctx context.Context, project, zone string, rs *install.RecordSet) error {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	cur := d.f.Records[recordKey(zone, rs.Name, rs.Type)]
	if cur == nil {
		return notFound("record set " + rs.Name)
	}
	cur.TTL, cur.Rrdatas = rs.TTL, append([]string(nil), rs.Rrdatas...)
	return nil
}

func (d dnsAPI) DeleteRecordSet(ctx context.Context, project, zone, name, typ string) error {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	k := recordKey(zone, name, typ)
	if d.f.Records[k] == nil {
		return notFound("record set " + name)
	}
	delete(d.f.Records, k)
	return nil
}

// Record is a record set and the zone it is in.
type Record struct {
	zone string
	install.RecordSet
}

// computeExtras is install.ComputeExtras: Private Google Access through
// the GCE fake, and routers, addresses and instance policies of its
// own.
type computeExtras struct{ f *Fake }

func (x computeExtras) SetPrivateIpGoogleAccess(ctx context.Context, project, region, subnet string, on bool) error {
	op, err := x.f.Compute.SetPrivateIpGoogleAccess(ctx, project, region, subnet, on)
	if err != nil {
		return err
	}
	_, err = gce.Await(ctx, x.f.Compute, project, op)
	return err
}

func regional(region, name string) string { return region + "/" + name }

func (x computeExtras) GetRouter(ctx context.Context, project, region, name string) (*compute.Router, error) {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	if err := x.f.need("compute.googleapis.com"); err != nil {
		return nil, err
	}
	if r := x.f.Routers[regional(region, name)]; r != nil {
		return cloneJSON(r), nil
	}
	return nil, notFound("router " + name)
}

func (x computeExtras) InsertRouter(ctx context.Context, project, region string, r *compute.Router) error {
	if _, err := x.f.Compute.GetNetwork(ctx, project, gce.LastSegment(r.Network)); err != nil {
		return badRequest("network " + r.Network + ": " + err.Error())
	}
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	k := regional(region, r.Name)
	if x.f.Routers[k] != nil {
		return conflict("router " + r.Name)
	}
	cp := cloneJSON(r)
	cp.Network = gce.SelfLinkBase + "projects/" + project + "/global/networks/" + gce.LastSegment(r.Network)
	cp.Region = region
	x.f.Routers[k] = cp
	return nil
}

func (x computeExtras) PatchRouter(ctx context.Context, project, region string, r *compute.Router) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	cur := x.f.Routers[regional(region, r.Name)]
	if cur == nil {
		return notFound("router " + r.Name)
	}
	cur.Nats = cloneJSON(&compute.Router{Nats: r.Nats}).Nats
	return nil
}

func (x computeExtras) DeleteRouter(ctx context.Context, project, region, name string) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	k := regional(region, name)
	if x.f.Routers[k] == nil {
		return notFound("router " + name)
	}
	delete(x.f.Routers, k)
	return nil
}

func (x computeExtras) GetAddress(ctx context.Context, project, region, name string) (*compute.Address, error) {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	if err := x.f.need("compute.googleapis.com"); err != nil {
		return nil, err
	}
	if a := x.f.Addresses[regional(region, name)]; a != nil {
		return cloneJSON(a), nil
	}
	return nil, notFound("address " + name)
}

// InsertAddress gives an internal address the first free one in its
// subnet after the subnet's gateway.
func (x computeExtras) InsertAddress(ctx context.Context, project, region string, a *compute.Address) error {
	sn, err := x.f.Compute.GetSubnetwork(ctx, project, region, gce.LastSegment(a.Subnetwork))
	if err != nil {
		return badRequest("subnetwork " + a.Subnetwork + ": " + err.Error())
	}
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	k := regional(region, a.Name)
	if x.f.Addresses[k] != nil {
		return conflict("address " + a.Name)
	}
	cp := cloneJSON(a)
	cp.Subnetwork = sn.SelfLink
	cp.Region = region
	if cp.Address == "" {
		ip := netip.MustParsePrefix(sn.IpCidrRange).Addr().Next()
		for taken := true; taken; {
			ip, taken = ip.Next(), false
			for _, o := range x.f.Addresses {
				taken = taken || o.Address == ip.String()
			}
		}
		cp.Address = ip.String()
	}
	x.f.Addresses[k] = cp
	return nil
}

func (x computeExtras) DeleteAddress(ctx context.Context, project, region, name string) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	k := regional(region, name)
	if x.f.Addresses[k] == nil {
		return notFound("address " + name)
	}
	delete(x.f.Addresses, k)
	return nil
}

// InstanceZone finds the instance in the GCE fake, in region's zones.
func (x computeExtras) InstanceZone(ctx context.Context, project, region, name string) (string, error) {
	zones, err := x.f.Compute.ListZones(ctx, project)
	if err != nil {
		return "", err
	}
	for _, z := range zones {
		if gce.LastSegment(z.Region) != region {
			continue
		}
		if _, err := x.f.Compute.GetInstance(ctx, project, z.Name, name); err == nil {
			return z.Name, nil
		}
	}
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	return "", nil
}

func (x computeExtras) GetInstanceIamPolicy(ctx context.Context, project, zone, name string) (*install.Policy, error) {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	return getPolicy(x.f.InstancePolicies[regional(zone, name)]), nil
}

func (x computeExtras) SetInstanceIamPolicy(ctx context.Context, project, zone, name string, p *install.Policy) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	k := regional(zone, name)
	cur := x.f.InstancePolicies[k]
	if err := x.f.setPolicy(&cur, p); err != nil {
		return err
	}
	x.f.InstancePolicies[k] = cur
	return nil
}

func cloneJSON[T any](v *T) *T {
	var out T
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &out)
	return &out
}
