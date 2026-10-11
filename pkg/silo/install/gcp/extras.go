package gcp

import (
	"context"
	"fmt"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/install"
	crm "google.golang.org/api/cloudresourcemanager/v1"
	compute "google.golang.org/api/compute/v1"
	dns "google.golang.org/api/dns/v1"
	iam "google.golang.org/api/iam/v1"
)

// policyVersion is the IAM policy version install reads and writes, so
// that conditional bindings survive a read-modify-write.
const policyVersion = 3

func (c ResourceManager) GetIamPolicy(ctx context.Context, project string) (*install.Policy, error) {
	p, err := c.s.Projects.GetIamPolicy(project, &crm.GetIamPolicyRequest{
		Options: &crm.GetPolicyOptions{RequestedPolicyVersion: policyVersion}}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := &install.Policy{Version: p.Version, Etag: p.Etag}
	for _, b := range p.Bindings {
		ib := &install.Binding{Role: b.Role, Members: b.Members}
		if b.Condition != nil {
			ib.Condition = &install.Condition{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		out.Bindings = append(out.Bindings, ib)
	}
	return out, nil
}

func (c ResourceManager) SetIamPolicy(ctx context.Context, project string, p *install.Policy) error {
	cp := &crm.Policy{Version: policyVersion, Etag: p.Etag}
	for _, b := range p.Bindings {
		cb := &crm.Binding{Role: b.Role, Members: b.Members}
		if b.Condition != nil {
			cb.Condition = &crm.Expr{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		cp.Bindings = append(cp.Bindings, cb)
	}
	// The default update mask is bindings and etag: audit configs are
	// left as they are.
	_, err := c.s.Projects.SetIamPolicy(project, &crm.SetIamPolicyRequest{Policy: cp}).Context(ctx).Do()
	return err
}

func saName(project, email string) string { return "projects/" + project + "/serviceAccounts/" + email }

func (c IAM) PatchServiceAccount(ctx context.Context, project string, sa *install.ServiceAccount) error {
	_, err := c.s.Projects.ServiceAccounts.Patch(saName(project, sa.Email), &iam.PatchServiceAccountRequest{
		ServiceAccount: &iam.ServiceAccount{DisplayName: sa.DisplayName, Description: sa.Description},
		UpdateMask:     "displayName,description",
	}).Context(ctx).Do()
	return err
}

func (c IAM) EnableServiceAccount(ctx context.Context, project, email string) error {
	_, err := c.s.Projects.ServiceAccounts.Enable(saName(project, email), &iam.EnableServiceAccountRequest{}).Context(ctx).Do()
	return err
}

func (c IAM) DeleteServiceAccount(ctx context.Context, project, email string) error {
	_, err := c.s.Projects.ServiceAccounts.Delete(saName(project, email)).Context(ctx).Do()
	return err
}

func roleName(project, id string) string { return "projects/" + project + "/roles/" + id }

func (c IAM) GetRole(ctx context.Context, project, id string) (*install.Role, error) {
	r, err := c.s.Projects.Roles.Get(roleName(project, id)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return &install.Role{Title: r.Title, Description: r.Description, Permissions: r.IncludedPermissions,
		Stage: r.Stage, Deleted: r.Deleted}, nil
}

func iamRole(r *install.Role) *iam.Role {
	return &iam.Role{Title: r.Title, Description: r.Description, IncludedPermissions: r.Permissions, Stage: r.Stage}
}

func (c IAM) CreateRole(ctx context.Context, project, id string, r *install.Role) error {
	_, err := c.s.Projects.Roles.Create("projects/"+project, &iam.CreateRoleRequest{RoleId: id, Role: iamRole(r)}).Context(ctx).Do()
	return err
}

func (c IAM) UpdateRole(ctx context.Context, project, id string, r *install.Role) error {
	name := roleName(project, id)
	cur, err := c.s.Projects.Roles.Get(name).Context(ctx).Do()
	if err != nil {
		return err
	}
	if cur.Deleted {
		if _, err := c.s.Projects.Roles.Undelete(name, &iam.UndeleteRoleRequest{Etag: cur.Etag}).Context(ctx).Do(); err != nil {
			return fmt.Errorf("undeleting %s: %w", name, err)
		}
	}
	_, err = c.s.Projects.Roles.Patch(name, iamRole(r)).UpdateMask("title,description,includedPermissions,stage").Context(ctx).Do()
	return err
}

func (c IAM) DeleteRole(ctx context.Context, project, id string) error {
	_, err := c.s.Projects.Roles.Delete(roleName(project, id)).Context(ctx).Do()
	return err
}

func (c DNS) PatchManagedZone(ctx context.Context, project string, z *install.ManagedZone) error {
	pv := &dns.ManagedZonePrivateVisibilityConfig{}
	for _, n := range z.Networks {
		pv.Networks = append(pv.Networks, &dns.ManagedZonePrivateVisibilityConfigNetwork{NetworkUrl: n})
	}
	_, err := c.s.ManagedZones.Patch(project, z.Name, &dns.ManagedZone{PrivateVisibilityConfig: pv}).Context(ctx).Do()
	return err
}

func (c DNS) DeleteManagedZone(ctx context.Context, project, name string) error {
	return c.s.ManagedZones.Delete(project, name).Context(ctx).Do()
}

func (c DNS) GetRecordSet(ctx context.Context, project, zone, name, typ string) (*install.RecordSet, error) {
	rs, err := c.s.ResourceRecordSets.Get(project, zone, name, typ).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return &install.RecordSet{Name: rs.Name, Type: rs.Type, TTL: rs.Ttl, Rrdatas: rs.Rrdatas}, nil
}

func dnsRecord(rs *install.RecordSet) *dns.ResourceRecordSet {
	return &dns.ResourceRecordSet{Name: rs.Name, Type: rs.Type, Ttl: rs.TTL, Rrdatas: rs.Rrdatas}
}

func (c DNS) CreateRecordSet(ctx context.Context, project, zone string, rs *install.RecordSet) error {
	_, err := c.s.ResourceRecordSets.Create(project, zone, dnsRecord(rs)).Context(ctx).Do()
	return err
}

func (c DNS) PatchRecordSet(ctx context.Context, project, zone string, rs *install.RecordSet) error {
	_, err := c.s.ResourceRecordSets.Patch(project, zone, rs.Name, rs.Type, dnsRecord(rs)).Context(ctx).Do()
	return err
}

func (c DNS) DeleteRecordSet(ctx context.Context, project, zone, name, typ string) error {
	_, err := c.s.ResourceRecordSets.Delete(project, zone, name, typ).Context(ctx).Do()
	return err
}

// ComputeExtras is install.ComputeExtras, over compute/v1 directly,
// with silod's client for waiting on operations.
type ComputeExtras struct {
	s   *compute.Service
	ops gce.Operations
}

// wait waits for the operation a call started and returns its failure.
func (c ComputeExtras) wait(ctx context.Context, project string, op *compute.Operation, err error) error {
	if err != nil {
		return err
	}
	done, err := gce.Await(ctx, c.ops, project, op)
	if err != nil {
		return err
	}
	if done.Error != nil && len(done.Error.Errors) > 0 {
		e := done.Error.Errors[0]
		return fmt.Errorf("%s %s: %s: %s", done.OperationType, gce.LastSegment(done.TargetLink), e.Code, e.Message)
	}
	return nil
}

func (c ComputeExtras) SetPrivateIpGoogleAccess(ctx context.Context, project, region, subnet string, on bool) error {
	op, err := c.s.Subnetworks.SetPrivateIpGoogleAccess(project, region, subnet, &compute.SubnetworksSetPrivateIpGoogleAccessRequest{
		PrivateIpGoogleAccess: on, ForceSendFields: []string{"PrivateIpGoogleAccess"}}).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) GetRouter(ctx context.Context, project, region, name string) (*compute.Router, error) {
	return c.s.Routers.Get(project, region, name).Context(ctx).Do()
}

func (c ComputeExtras) InsertRouter(ctx context.Context, project, region string, r *compute.Router) error {
	op, err := c.s.Routers.Insert(project, region, r).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) PatchRouter(ctx context.Context, project, region string, r *compute.Router) error {
	op, err := c.s.Routers.Patch(project, region, r.Name, &compute.Router{Nats: r.Nats, ForceSendFields: []string{"Nats"}}).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) DeleteRouter(ctx context.Context, project, region, name string) error {
	op, err := c.s.Routers.Delete(project, region, name).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) GetAddress(ctx context.Context, project, region, name string) (*compute.Address, error) {
	return c.s.Addresses.Get(project, region, name).Context(ctx).Do()
}

func (c ComputeExtras) InsertAddress(ctx context.Context, project, region string, a *compute.Address) error {
	op, err := c.s.Addresses.Insert(project, region, a).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) DeleteAddress(ctx context.Context, project, region, name string) error {
	op, err := c.s.Addresses.Delete(project, region, name).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) InstanceZone(ctx context.Context, project, region, name string) (string, error) {
	// No region (uninstall with no install record has no home region):
	// nothing to look in. regions.get of "" would get GCE's region list,
	// whose string id doesn't decode as a Region's.
	if region == "" {
		return "", nil
	}
	r, err := c.s.Regions.Get(project, region).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	for _, z := range r.Zones {
		zone := gce.LastSegment(z)
		_, err := c.s.Instances.Get(project, zone, name).Context(ctx).Do()
		if err == nil {
			return zone, nil
		}
		if !install.IsNotFound(err) {
			return "", err
		}
	}
	return "", nil
}

func (c ComputeExtras) GetInstanceIamPolicy(ctx context.Context, project, zone, name string) (*install.Policy, error) {
	p, err := c.s.Instances.GetIamPolicy(project, zone, name).OptionsRequestedPolicyVersion(policyVersion).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := &install.Policy{Version: p.Version, Etag: p.Etag}
	for _, b := range p.Bindings {
		ib := &install.Binding{Role: b.Role, Members: b.Members}
		if b.Condition != nil {
			ib.Condition = &install.Condition{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		out.Bindings = append(out.Bindings, ib)
	}
	return out, nil
}

func (c ComputeExtras) SetInstanceIamPolicy(ctx context.Context, project, zone, name string, p *install.Policy) error {
	cp := &compute.Policy{Version: policyVersion, Etag: p.Etag}
	for _, b := range p.Bindings {
		cb := &compute.Binding{Role: b.Role, Members: b.Members}
		if b.Condition != nil {
			cb.Condition = &compute.Expr{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		cp.Bindings = append(cp.Bindings, cb)
	}
	_, err := c.s.Instances.SetIamPolicy(project, zone, name, &compute.ZoneSetPolicyRequest{Policy: cp}).Context(ctx).Do()
	return err
}

func (c ComputeExtras) GetHealthCheck(ctx context.Context, project, region, name string) (*compute.HealthCheck, error) {
	return c.s.RegionHealthChecks.Get(project, region, name).Context(ctx).Do()
}

func (c ComputeExtras) InsertHealthCheck(ctx context.Context, project, region string, hc *compute.HealthCheck) error {
	op, err := c.s.RegionHealthChecks.Insert(project, region, hc).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) DeleteHealthCheck(ctx context.Context, project, region, name string) error {
	op, err := c.s.RegionHealthChecks.Delete(project, region, name).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) GetInstanceGroupManager(ctx context.Context, project, region, name string) (*compute.InstanceGroupManager, error) {
	return c.s.RegionInstanceGroupManagers.Get(project, region, name).Context(ctx).Do()
}

func (c ComputeExtras) InsertInstanceGroupManager(ctx context.Context, project, region string, m *compute.InstanceGroupManager) error {
	op, err := c.s.RegionInstanceGroupManagers.Insert(project, region, m).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) PatchInstanceGroupManager(ctx context.Context, project, region string, m *compute.InstanceGroupManager) error {
	op, err := c.s.RegionInstanceGroupManagers.Patch(project, region, m.Name, &compute.InstanceGroupManager{
		InstanceTemplate: m.InstanceTemplate, TargetSize: m.TargetSize, UpdatePolicy: m.UpdatePolicy,
		AutoHealingPolicies: m.AutoHealingPolicies, ForceSendFields: []string{"TargetSize"}}).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) DeleteInstanceGroupManager(ctx context.Context, project, region, name string) error {
	op, err := c.s.RegionInstanceGroupManagers.Delete(project, region, name).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) GetBackendService(ctx context.Context, project, region, name string) (*compute.BackendService, error) {
	return c.s.RegionBackendServices.Get(project, region, name).Context(ctx).Do()
}

func (c ComputeExtras) InsertBackendService(ctx context.Context, project, region string, b *compute.BackendService) error {
	op, err := c.s.RegionBackendServices.Insert(project, region, b).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) PatchBackendService(ctx context.Context, project, region string, b *compute.BackendService) error {
	op, err := c.s.RegionBackendServices.Patch(project, region, b.Name, &compute.BackendService{
		Backends: b.Backends, ForceSendFields: []string{"Backends"}}).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) DeleteBackendService(ctx context.Context, project, region, name string) error {
	op, err := c.s.RegionBackendServices.Delete(project, region, name).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) GetInstanceGroup(ctx context.Context, project, zone, name string) (*compute.InstanceGroup, error) {
	return c.s.InstanceGroups.Get(project, zone, name).Context(ctx).Do()
}

func (c ComputeExtras) InsertInstanceGroup(ctx context.Context, project, zone string, g *compute.InstanceGroup) error {
	op, err := c.s.InstanceGroups.Insert(project, zone, g).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) GroupInstances(ctx context.Context, project, zone, name string) ([]string, error) {
	var out []string
	err := c.s.InstanceGroups.ListInstances(project, zone, name, &compute.InstanceGroupsListInstancesRequest{InstanceState: "ALL"}).
		Pages(ctx, func(l *compute.InstanceGroupsListInstances) error {
			for _, it := range l.Items {
				out = append(out, gce.LastSegment(it.Instance))
			}
			return nil
		})
	return out, err
}

func (c ComputeExtras) AddGroupInstances(ctx context.Context, project, zone, name string, instances []string) error {
	req := &compute.InstanceGroupsAddInstancesRequest{}
	for _, in := range instances {
		req.Instances = append(req.Instances, &compute.InstanceReference{Instance: "projects/" + project + "/zones/" + zone + "/instances/" + in})
	}
	op, err := c.s.InstanceGroups.AddInstances(project, zone, name, req).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) DeleteInstanceGroup(ctx context.Context, project, zone, name string) error {
	op, err := c.s.InstanceGroups.Delete(project, zone, name).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}
