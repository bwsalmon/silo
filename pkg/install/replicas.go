package install

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/gateway"
	"github.com/bwsalmon/silo/pkg/gwtls"
	compute "google.golang.org/api/compute/v1"
)

// The replica groups' objects, by fixed name (install.md step 12,
// gateway.md "Placement", SV-89).
const (
	// AnchorGroup is the unmanaged instance group that puts silo-control
	// behind the control NLB beside the MIG's VMs.
	AnchorGroup = "silo-control-anchor"
	// SilodAddressKey is a gateway VM's metadata key holding the address
	// it reaches silod on: the control NLB's, or silo-control's.
	SilodAddressKey = "silo-silod-address"
	// healthDelay is how long a new VM has to start serving before
	// autohealing judges it.
	healthDelay = 300
)

// group is one replica group: the control VMs' or the gateway VMs'.
type group struct {
	Role string // gwtls.RoleControl or gwtls.RoleGateway
	// Names: the MIG and its VMs' base name, the health check, the
	// backend service and forwarding rule, and the NLB's address.
	MIG, Base, HealthCheck, LB, Address string
}

var (
	controlGroup = group{Role: gwtls.RoleControl, MIG: "silo-control-mig", Base: "silo-control",
		HealthCheck: "silo-control-hc", LB: "silo-control-lb", Address: "silo-control-lb-ip"}
	gatewayGroup = group{Role: gwtls.RoleGateway, MIG: "silo-gateway-mig", Base: "silo-gateway",
		HealthCheck: "silo-gateway-hc", LB: "silo-gateway-lb", Address: "silo-gateway-lb-ip"}
)

// Replicas is step 12's replica groups, part of the control step: with --control-replicas above 1, a
// regional MIG of N-1 more control VMs from an instance template, and
// with silo-control (in AnchorGroup) behind an internal passthrough NLB
// with global access and a health check; with --gateway-replicas N, a
// regional MIG of N gateway-only VMs, with no service account, behind
// one of their own. silo.internal moves to the gateway NLB's address if
// there is one, else to the control NLB's. A MIG rolls a new template
// out proactively, with no VM unavailable.
//
// silo-control stays a VM of its own, not a MIG member: it is the VM
// the front door's IAP tunnel, its IAM grant and the root and admin
// steps name, and a MIG's VMs are replaced, with new names, by every
// upgrade.
type Replicas struct{ Env *Env }

// GroupObserved is one group's objects as GCE has them.
type GroupObserved struct {
	// Template is the name of the template the flags call for, and
	// TemplateExists whether it exists.
	Template       string `json:"template,omitempty"`
	TemplateExists bool   `json:"templateExists,omitempty"`
	// The MIG's, if MIG.
	MIG         bool   `json:"mig,omitempty"`
	MIGOurs     bool   `json:"migOurs,omitempty"`
	MIGTemplate string `json:"migTemplate,omitempty"`
	MIGSize     int64  `json:"migSize,omitempty"`
	MIGSurge    int64  `json:"migSurge,omitempty"`
	HealthCheck bool   `json:"healthCheck,omitempty"`
	// Backends are the backend service's groups, by name, if LB.
	LB       bool     `json:"lb,omitempty"`
	Backends []string `json:"backends,omitempty"`
	Rule     bool     `json:"rule,omitempty"`
	RuleIP   string   `json:"ruleIP,omitempty"`
	// AddressIP is the NLB's reserved address, "" if none.
	AddressIP string `json:"addressIP,omitempty"`
	// Templates are every template of this group's install has made.
	Templates []string `json:"templates,omitempty"`
}

// ReplicasObserved is both groups, and what they are built from.
type ReplicasObserved struct {
	ComputeEnabled bool          `json:"computeEnabled"`
	Zones          []string      `json:"zones,omitempty"`
	AnchorZone     string        `json:"anchorZone,omitempty"`
	Anchor         bool          `json:"anchor,omitempty"`
	AnchorMembers  []string      `json:"anchorMembers,omitempty"`
	Control        GroupObserved `json:"control"`
	Gateway        GroupObserved `json:"gateway"`
	// Record is silo.internal.'s A record now, "" if none.
	Record    string `json:"record,omitempty"`
	GatewayIP string `json:"gatewayIP,omitempty"`
	// From the control step's observation: what the VMs run.
	Digest string `json:"digest,omitempty"`
	// HarnessDigest is the harness image's, with --agent-sessions.
	HarnessDigest string `json:"harnessDigest,omitempty"`
	DBConnection  string `json:"dbConnection,omitempty"`
	// CredentialKey is whether silo-control has the agent credentials'
	// key, which control MIG VMs share (the key stays out of the plan).
	CredentialKey bool `json:"credentialKey,omitempty"`
}

func (s *Replicas) wantControl() bool { return s.Env.Config.ControlReplicas > 1 }
func (s *Replicas) wantGateway() bool { return s.Env.Config.GatewayReplicas > 0 }

// Observe reads both groups, given silo-control's observation.
func (s *Replicas) Observe(ctx context.Context, c *ControlObserved) (*ReplicasObserved, error) {
	obs := &ReplicasObserved{}
	obs.Digest, obs.DBConnection, obs.GatewayIP, obs.AnchorZone = c.Digest, c.DBConnection, c.GatewayIP, c.Zone
	obs.HarnessDigest = c.HarnessDigest
	obs.CredentialKey = c.Metadata[CredentialKeyKey] != ""
	if !c.ComputeEnabled {
		return obs, nil
	}
	obs.ComputeEnabled = true
	cfg := &s.Env.Config
	project, region := cfg.Project, cfg.HomeRegion()
	x := s.Env.Cloud.ComputeExtras
	r, err := s.Env.Cloud.Compute.GetRegion(ctx, project, region)
	if err != nil {
		return nil, err
	}
	for _, z := range r.Zones {
		obs.Zones = append(obs.Zones, gce.LastSegment(z))
	}
	sort.Strings(obs.Zones)
	if obs.AnchorZone != "" {
		members, err := x.GroupInstances(ctx, project, obs.AnchorZone, AnchorGroup)
		switch {
		case IsNotFound(err):
		case err != nil:
			return nil, err
		default:
			obs.Anchor, obs.AnchorMembers = true, members
		}
	}
	for _, g := range []struct {
		g   group
		out *GroupObserved
	}{{controlGroup, &obs.Control}, {gatewayGroup, &obs.Gateway}} {
		if err := s.observeGroup(ctx, g.g, obs, g.out); err != nil {
			return nil, err
		}
	}
	if on, err := s.Env.Enabled(ctx, "dns.googleapis.com"); err != nil {
		return nil, err
	} else if on {
		rs, err := s.Env.Cloud.DNS.GetRecordSet(ctx, project, ZoneName, ZoneDNSName, "A")
		switch {
		case IsNotFound(err):
		case err != nil:
			return nil, err
		case len(rs.Rrdatas) > 0:
			obs.Record = rs.Rrdatas[0]
		}
	}
	return obs, nil
}

func (s *Replicas) observeGroup(ctx context.Context, g group, obs *ReplicasObserved, out *GroupObserved) error {
	cfg := &s.Env.Config
	project, region := cfg.Project, cfg.HomeRegion()
	x := s.Env.Cloud.ComputeExtras
	if a, err := x.GetAddress(ctx, project, region, g.Address); err == nil {
		out.AddressIP = a.Address
	} else if !IsNotFound(err) {
		return err
	}
	// Templates carry no labels of their own: found by name, and kept
	// to install's own by their description.
	ts, err := s.Env.Cloud.Compute.ListInstanceTemplates(ctx, project, region, gce.ListFilter{NamePrefix: g.Base + "-"})
	if err != nil {
		return err
	}
	for _, t := range ts {
		if id, ok := ownedBy(t.Description); ok && id == s.Env.ID && t.Properties != nil && t.Properties.Labels[gwtls.LabelRole] == g.Role {
			out.Templates = append(out.Templates, t.Name)
		}
	}
	sort.Strings(out.Templates)
	if t := s.template(g, obs, out); t != nil {
		out.Template = t.Name
		for _, n := range out.Templates {
			out.TemplateExists = out.TemplateExists || n == t.Name
		}
	}
	if m, err := x.GetInstanceGroupManager(ctx, project, region, g.MIG); err == nil {
		_, ours := ownedBy(m.Description)
		out.MIG, out.MIGOurs, out.MIGTemplate, out.MIGSize = true, ours, gce.LastSegment(m.InstanceTemplate), m.TargetSize
		if m.UpdatePolicy != nil && m.UpdatePolicy.MaxSurge != nil {
			out.MIGSurge = m.UpdatePolicy.MaxSurge.Fixed
		}
	} else if !IsNotFound(err) {
		return err
	}
	if _, err := x.GetHealthCheck(ctx, project, region, g.HealthCheck); err == nil {
		out.HealthCheck = true
	} else if !IsNotFound(err) {
		return err
	}
	if b, err := x.GetBackendService(ctx, project, region, g.LB); err == nil {
		out.LB = true
		for _, be := range b.Backends {
			out.Backends = append(out.Backends, gce.LastSegment(be.Group))
		}
		sort.Strings(out.Backends)
	} else if !IsNotFound(err) {
		return err
	}
	if r, err := x.GetForwardingRule(ctx, project, region, g.LB); err == nil {
		out.Rule, out.RuleIP = true, r.IPAddress
	} else if !IsNotFound(err) {
		return err
	}
	return nil
}

// FrontAddress is the address silo.internal names: the gateway NLB's,
// else the control NLB's, else silo-gateway-ip's, by which of them has
// a forwarding rule. The dns step and this one agree on it.
func FrontAddress(gwRule, ctlRule, gatewayIP string) string {
	switch {
	case gwRule != "":
		return gwRule
	case ctlRule != "":
		return ctlRule
	}
	return gatewayIP
}

// frontAddress is FrontAddress once this step has done its work.
func (s *Replicas) frontAddress(obs *ReplicasObserved) string {
	gw, ctl := "", ""
	if s.wantGateway() {
		gw = obs.Gateway.AddressIP
	}
	if s.wantControl() {
		ctl = obs.Control.AddressIP
	}
	if s.wantGateway() && gw == "" || s.wantControl() && ctl == "" {
		return "" // not known until the address is reserved
	}
	return FrontAddress(gw, ctl, obs.GatewayIP)
}

// silodAddress is where gateway VMs reach silod: the control NLB, or
// silo-control.
func (s *Replicas) silodAddress(obs *ReplicasObserved) string {
	if s.wantControl() {
		return obs.Control.AddressIP
	}
	return obs.GatewayIP
}

func (s *Replicas) size(g group) int64 {
	if g.Role == gwtls.RoleControl {
		return int64(s.Env.Config.ControlReplicas - 1)
	}
	return int64(s.Env.Config.GatewayReplicas)
}

// surge is the rollout's maxSurge: one VM at a time is what install.md
// asks, but GCE refuses a fixed maxSurge on a regional MIG below the
// number of its zones, so it is that, with none unavailable.
func surge(obs *ReplicasObserved) int64 {
	if n := int64(len(obs.Zones)); n > 1 {
		return n
	}
	return 1
}

// template is the group's instance template as the flags call for it,
// named for a hash of its properties (templates are immutable), nil if
// what it needs (the image, the database, silod's address) isn't there
// yet.
func (s *Replicas) template(g group, obs *ReplicasObserved, out *GroupObserved) *compute.InstanceTemplate {
	cfg := &s.Env.Config
	project, region := cfg.Project, cfg.HomeRegion()
	if obs.Digest == "" {
		return nil
	}
	p := &compute.InstanceProperties{
		MachineType: cfg.ControlMachineType,
		Labels:      map[string]string{LabelKey: s.Env.ID, gwtls.LabelRole: g.Role},
		Disks: []*compute.AttachedDisk{{Boot: true, AutoDelete: true,
			InitializeParams: &compute.AttachedDiskInitializeParams{SourceImage: cosImage, DiskSizeGb: controlDiskGB}}},
		NetworkInterfaces: []*compute.NetworkInterface{{
			Network:    gce.SelfLinkBase + networkPath(project),
			Subnetwork: gce.SelfLinkBase + subnetPath(project, region, GatewaySubnetName),
		}},
		ShieldedInstanceConfig: &compute.ShieldedInstanceConfig{EnableSecureBoot: true, EnableVtpm: true, EnableIntegrityMonitoring: true},
	}
	md := map[string]string{
		ImageKey:                 DeployedImage(cfg, obs.Digest),
		"google-logging-enabled": "true",
		"block-project-ssh-keys": "TRUE",
		"enable-oslogin":         "TRUE",
	}
	switch g.Role {
	case gwtls.RoleControl:
		if obs.DBConnection == "" {
			return nil
		}
		md[userDataKey] = (&Control{Env: s.Env}).userData(&ControlObserved{DBConnection: obs.DBConnection, HarnessDigest: obs.HarnessDigest})
		p.ServiceAccounts = []*compute.ServiceAccount{{Email: SilodEmail(project),
			Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}}}
	case gwtls.RoleGateway:
		addr := s.silodAddress(obs)
		if addr == "" {
			return nil
		}
		md[userDataKey] = GatewayUserData(GatewayParams{Region: region, AddressSpace: cfg.AddressSpace})
		md[SilodAddressKey] = addr
		// No service account at all: no token on the VM, for any
		// process. The serial console is output only: the leader reads
		// the certificate request there.
		md["serial-port-enable"] = "FALSE"
	}
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	p.Metadata = &compute.Metadata{}
	for _, k := range keys {
		p.Metadata.Items = append(p.Metadata.Items, &compute.MetadataItems{Key: k, Value: ptr(md[k])})
	}
	b, _ := json.Marshal(p)
	return &compute.InstanceTemplate{
		Name:        g.Base + "-" + userDataHash(string(b))[:10],
		Description: owned(s.Env, "the "+g.Role+" MIG's VMs"),
		Properties:  p,
	}
}

// Plan is nil when there are no replicas and none are wanted.
func (s *Replicas) Plan(obs *ReplicasObserved) []Change {
	if !s.wantControl() && !s.wantGateway() && !s.anything(obs) {
		return nil
	}
	var out []Change
	out = append(out, s.planGroup(controlGroup, s.wantControl(), obs, &obs.Control)...)
	out = append(out, s.planGroup(gatewayGroup, s.wantGateway(), obs, &obs.Gateway)...)
	if want := s.frontAddress(obs); want == "" || want != obs.Record {
		target := want
		if target == "" {
			target = "the NLB's address"
		}
		out = append(out, Change{Action: ActionUpdate, Object: ZoneDNSName, Detail: "A " + target, Estimate: 5 * time.Second})
	}
	return out
}

// anything reports whether either group has anything left.
func (s *Replicas) anything(obs *ReplicasObserved) bool {
	for _, g := range []*GroupObserved{&obs.Control, &obs.Gateway} {
		if g.MIG || g.LB || g.Rule || g.HealthCheck || g.AddressIP != "" || len(g.Templates) > 0 {
			return true
		}
	}
	return obs.Anchor
}

func (s *Replicas) planGroup(g group, want bool, obs *ReplicasObserved, o *GroupObserved) []Change {
	if !want {
		return s.planRemoveGroup(g, obs, o)
	}
	var out []Change
	add := func(a Action, object, detail string, d time.Duration) {
		out = append(out, Change{Action: a, Object: object, Detail: detail, Estimate: d})
	}
	if o.MIG && !o.MIGOurs {
		return []Change{notOurs(s.Env, g.MIG, "the "+g.Role+" MIG is install's",
			fmt.Sprintf("gcloud compute instance-groups managed delete %s --region %s", g.MIG, s.Env.Config.HomeRegion()))}
	}
	if o.AddressIP == "" {
		add(ActionCreate, g.Address, "internal, in "+GatewaySubnetName, 5*time.Second)
	}
	tmpl := o.Template
	if tmpl == "" {
		tmpl = g.Base + "-<hash>"
	}
	if !o.TemplateExists {
		sa := "the silod service account"
		if g.Role == gwtls.RoleGateway {
			sa = "no service account"
		}
		add(ActionCreate, tmpl, fmt.Sprintf("instance template: %s cos-stable, %s, %s", s.Env.Config.ControlMachineType, sa, shortImage(DeployedImage(&s.Env.Config, obs.Digest))), 5*time.Second)
	}
	if !o.HealthCheck {
		add(ActionCreate, g.HealthCheck, fmt.Sprintf("TCP %d", gateway.APIPort), 5*time.Second)
	}
	size := s.size(g)
	switch {
	case !o.MIG:
		add(ActionCreate, g.MIG, fmt.Sprintf("regional MIG of %d, %s", size, tmpl), 3*time.Minute)
	case o.MIGTemplate != tmpl || o.MIGSize != size || o.MIGSurge != surge(obs):
		add(ActionUpdate, g.MIG, fmt.Sprintf("%d VMs of %s (rolls out with up to %d new VMs at a time, none unavailable)", size, tmpl, surge(obs)), 5*time.Minute)
	}
	if g.Role == gwtls.RoleControl && obs.AnchorZone != "" && !(obs.Anchor && contains(obs.AnchorMembers, ControlVM)) {
		add(ActionCreate, AnchorGroup, ControlVM+", behind "+g.LB, 10*time.Second)
	}
	backends := s.backends(g, obs)
	switch {
	case !o.LB:
		add(ActionCreate, g.LB+" (backend service)", "internal passthrough, TCP, "+strings.Join(backends, ", "), 10*time.Second)
	case strings.Join(o.Backends, ",") != strings.Join(backends, ","):
		add(ActionUpdate, g.LB+" (backend service)", "backends "+strings.Join(backends, ", "), 10*time.Second)
	}
	if !o.Rule {
		addr := o.AddressIP
		if addr == "" {
			addr = g.Address
		}
		add(ActionCreate, g.LB+" (forwarding rule)", addr+", all TCP ports, global access", 10*time.Second)
	}
	if len(out) == 0 {
		add(ActionOK, g.MIG, fmt.Sprintf("%d VMs of %s behind %s", size, tmpl, o.RuleIP), 0)
	}
	// Templates no MIG uses any more.
	for _, t := range o.Templates {
		if t != tmpl && t != o.MIGTemplate {
			add(ActionDelete, t, "instance template no longer used", 5*time.Second)
		}
	}
	return out
}

// backends are the group's backend service's groups, by name.
func (s *Replicas) backends(g group, obs *ReplicasObserved) []string {
	out := []string{g.MIG}
	if g.Role == gwtls.RoleControl {
		out = append(out, AnchorGroup)
	}
	sort.Strings(out)
	return out
}

// planRemoveGroup is a group the flags don't want, or uninstall's.
func (s *Replicas) planRemoveGroup(g group, obs *ReplicasObserved, o *GroupObserved) []Change {
	var out []Change
	del := func(object, detail string) {
		out = append(out, Change{Action: ActionDelete, Object: object, Detail: detail, Estimate: 10 * time.Second})
	}
	if o.Rule {
		del(g.LB+" (forwarding rule)", o.RuleIP)
	}
	if o.LB {
		del(g.LB+" (backend service)", "")
	}
	if g.Role == gwtls.RoleControl && obs.Anchor {
		del(AnchorGroup, "")
	}
	if o.MIG && o.MIGOurs {
		del(g.MIG, fmt.Sprintf("and its %d VMs", o.MIGSize))
	}
	if o.HealthCheck {
		del(g.HealthCheck, "")
	}
	for _, t := range o.Templates {
		del(t, "instance template")
	}
	if o.AddressIP != "" {
		del(g.Address, o.AddressIP)
	}
	return out
}

func (s *Replicas) PlanRemove(obs *ReplicasObserved) []Change {
	return append(s.planRemoveGroup(controlGroup, obs, &obs.Control), s.planRemoveGroup(gatewayGroup, obs, &obs.Gateway)...)
}

// observe is Observe with silo-control observed afresh.
func (s *Replicas) observe(ctx context.Context) (*ReplicasObserved, error) {
	c, err := (&Control{Env: s.Env}).observeVM(ctx)
	if err != nil {
		return nil, err
	}
	return s.Observe(ctx, c)
}

// Apply makes the groups what the flags call for, or, with remove
// (uninstall), takes them down.
func (s *Replicas) Apply(ctx context.Context, remove bool) error {
	obs, err := s.observe(ctx)
	if err != nil {
		return err
	}
	// The gateway group first when building it: it may need the control
	// NLB's address, made just before. Removal goes the other way.
	if s.wantControl() && !remove {
		if err := s.applyGroup(ctx, controlGroup, obs, &obs.Control); err != nil {
			return err
		}
		if obs, err = s.observe(ctx); err != nil {
			return err
		}
	}
	if s.wantGateway() && !remove {
		if err := s.applyGroup(ctx, gatewayGroup, obs, &obs.Gateway); err != nil {
			return err
		}
		if obs, err = s.observe(ctx); err != nil {
			return err
		}
	}
	// silo.internal moves before anything it named is taken down.
	if want := FrontAddress(ifs(s.wantGateway() && !remove, obs.Gateway.RuleIP), ifs(s.wantControl() && !remove, obs.Control.RuleIP), obs.GatewayIP); want != "" && want != obs.Record {
		if err := s.Env.Cloud.DNS.PatchRecordSet(ctx, s.Env.Config.Project, ZoneName,
			&RecordSet{Name: ZoneDNSName, Type: "A", TTL: recordTTL, Rrdatas: []string{want}}); err != nil {
			return fmt.Errorf("moving %s to %s: %w", ZoneDNSName, want, err)
		}
	}
	for _, g := range []struct {
		g    group
		o    *GroupObserved
		want bool
	}{{gatewayGroup, &obs.Gateway, s.wantGateway()}, {controlGroup, &obs.Control, s.wantControl()}} {
		if g.want && !remove {
			if err := s.deleteTemplates(ctx, g.o, g.o.Template, g.o.MIGTemplate); err != nil {
				return err
			}
			continue
		}
		if err := s.removeGroup(ctx, g.g, obs, g.o); err != nil {
			return err
		}
	}
	return nil
}

func ifs(b bool, s string) string {
	if b {
		return s
	}
	return ""
}

func ignoreExists(err error) error {
	if IsAlreadyExists(err) {
		return nil
	}
	return err
}

func ignoreNotFound(err error) error {
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Replicas) applyGroup(ctx context.Context, g group, obs *ReplicasObserved, o *GroupObserved) error {
	cfg := &s.Env.Config
	project, region := cfg.Project, cfg.HomeRegion()
	x := s.Env.Cloud.ComputeExtras
	if o.AddressIP == "" {
		if err := ignoreExists(x.InsertAddress(ctx, project, region, &compute.Address{
			Name: g.Address, Description: owned(s.Env, "the "+g.Role+" NLB's address"), AddressType: "INTERNAL",
			Subnetwork: subnetPath(project, region, GatewaySubnetName), Labels: map[string]string{LabelKey: s.Env.ID},
		})); err != nil {
			return fmt.Errorf("reserving %s: %w", g.Address, err)
		}
		a, err := x.GetAddress(ctx, project, region, g.Address)
		if err != nil {
			return err
		}
		o.AddressIP = a.Address
	}
	t := s.template(g, obs, o)
	if t == nil {
		return fmt.Errorf("%s: the image, the database or silod's address isn't there yet", g.MIG)
	}
	if g.Role == gwtls.RoleControl && cfg.AgentSessions > 0 {
		// The MIG's VMs hold the same key to the agent credentials as
		// silo-control (agents.md, "Credentials").
		key, err := s.credentialKey(ctx, obs)
		if err != nil {
			return err
		}
		t.Properties.Metadata.Items = append(t.Properties.Metadata.Items, &compute.MetadataItems{Key: CredentialKeyKey, Value: ptr(key)})
	}
	c := s.Env.Cloud.Compute
	if !contains(o.Templates, t.Name) {
		op, err := c.InsertInstanceTemplate(ctx, project, region, t)
		if err == nil {
			err = await(ctx, c, project, op)
		}
		if err := ignoreExists(err); err != nil {
			return fmt.Errorf("creating %s: %w", t.Name, err)
		}
	}
	hcURL := "projects/" + project + "/regions/" + region + "/healthChecks/" + g.HealthCheck
	if !o.HealthCheck {
		if err := ignoreExists(x.InsertHealthCheck(ctx, project, region, &compute.HealthCheck{
			Name: g.HealthCheck, Description: owned(s.Env, "the "+g.Role+" VMs' gateway"), Type: "TCP",
			CheckIntervalSec: 5, TimeoutSec: 5, HealthyThreshold: 2, UnhealthyThreshold: 3,
			TcpHealthCheck: &compute.TCPHealthCheck{Port: gateway.APIPort},
		})); err != nil {
			return fmt.Errorf("creating %s: %w", g.HealthCheck, err)
		}
	}
	m := &compute.InstanceGroupManager{
		Name: g.MIG, Description: owned(s.Env, "the "+g.Role+" VMs"), BaseInstanceName: g.Base,
		InstanceTemplate: "projects/" + project + "/regions/" + region + "/instanceTemplates/" + t.Name,
		TargetSize:       s.size(g),
		UpdatePolicy: &compute.InstanceGroupManagerUpdatePolicy{Type: "PROACTIVE", MinimalAction: "REPLACE",
			ReplacementMethod: "SUBSTITUTE",
			MaxSurge:          &compute.FixedOrPercent{Fixed: surge(obs)},
			MaxUnavailable:    &compute.FixedOrPercent{Fixed: 0, ForceSendFields: []string{"Fixed"}}},
		AutoHealingPolicies: []*compute.InstanceGroupManagerAutoHealingPolicy{{HealthCheck: hcURL, InitialDelaySec: healthDelay}},
		ForceSendFields:     []string{"TargetSize"},
	}
	if !o.MIG {
		if err := ignoreExists(x.InsertInstanceGroupManager(ctx, project, region, m)); err != nil {
			return fmt.Errorf("creating %s: %w", g.MIG, err)
		}
	} else if o.MIGTemplate != t.Name || o.MIGSize != m.TargetSize || o.MIGSurge != surge(obs) {
		if err := x.PatchInstanceGroupManager(ctx, project, region, m); err != nil {
			return fmt.Errorf("updating %s: %w", g.MIG, err)
		}
	}
	backends := []*compute.Backend{{Group: "projects/" + project + "/regions/" + region + "/instanceGroups/" + g.MIG, BalancingMode: "CONNECTION"}}
	if g.Role == gwtls.RoleControl {
		if obs.AnchorZone == "" {
			return fmt.Errorf("there is no VM %s: the control step makes it", ControlVM)
		}
		if !obs.Anchor {
			if err := ignoreExists(x.InsertInstanceGroup(ctx, project, obs.AnchorZone, &compute.InstanceGroup{
				Name: AnchorGroup, Description: owned(s.Env, ControlVM+", behind the control NLB"),
				Network: gce.SelfLinkBase + networkPath(project),
			})); err != nil {
				return fmt.Errorf("creating %s: %w", AnchorGroup, err)
			}
		}
		if !contains(obs.AnchorMembers, ControlVM) {
			if err := x.AddGroupInstances(ctx, project, obs.AnchorZone, AnchorGroup, []string{ControlVM}); err != nil && !IsAlreadyExists(err) {
				return fmt.Errorf("adding %s to %s: %w", ControlVM, AnchorGroup, err)
			}
		}
		backends = append(backends, &compute.Backend{Group: "projects/" + project + "/zones/" + obs.AnchorZone + "/instanceGroups/" + AnchorGroup, BalancingMode: "CONNECTION"})
	}
	bs := &compute.BackendService{Name: g.LB, Description: owned(s.Env, "the "+g.Role+" NLB"),
		LoadBalancingScheme: "INTERNAL", Protocol: "TCP", HealthChecks: []string{hcURL}, Backends: backends,
		Network: gce.SelfLinkBase + networkPath(project)}
	if !o.LB {
		if err := ignoreExists(x.InsertBackendService(ctx, project, region, bs)); err != nil {
			return fmt.Errorf("creating %s: %w", g.LB, err)
		}
	} else if strings.Join(o.Backends, ",") != strings.Join(s.backends(g, obs), ",") {
		if err := x.PatchBackendService(ctx, project, region, bs); err != nil {
			return fmt.Errorf("updating %s: %w", g.LB, err)
		}
	}
	if !o.Rule {
		// A passthrough NLB delivers the caller's packets as they were
		// sent, source address and all, which origin identity reads.
		if err := ignoreExists(x.InsertForwardingRule(ctx, project, region, &compute.ForwardingRule{
			Name: g.LB, Description: owned(s.Env, "the "+g.Role+" NLB"),
			LoadBalancingScheme: "INTERNAL", IPProtocol: "TCP", AllPorts: true, AllowGlobalAccess: true,
			IPAddress:      "projects/" + project + "/regions/" + region + "/addresses/" + g.Address,
			BackendService: "projects/" + project + "/regions/" + region + "/backendServices/" + g.LB,
			Network:        gce.SelfLinkBase + networkPath(project),
			Subnetwork:     gce.SelfLinkBase + subnetPath(project, region, GatewaySubnetName),
		})); err != nil {
			return fmt.Errorf("creating %s: %w", g.LB, err)
		}
	}
	return nil
}

// credentialKey reads silo-control's agent credentials key.
func (s *Replicas) credentialKey(ctx context.Context, obs *ReplicasObserved) (string, error) {
	vm, err := s.Env.Cloud.Compute.GetInstance(ctx, s.Env.Config.Project, obs.AnchorZone, ControlVM)
	if err != nil {
		return "", err
	}
	if vm.Metadata != nil {
		for _, it := range vm.Metadata.Items {
			if it.Key == CredentialKeyKey && it.Value != nil {
				return *it.Value, nil
			}
		}
	}
	return "", fmt.Errorf("%s has no %s yet: the control step sets it", ControlVM, CredentialKeyKey)
}

// deleteTemplates deletes the group's templates but keep.
func (s *Replicas) deleteTemplates(ctx context.Context, o *GroupObserved, keep ...string) error {
	c, project, region := s.Env.Cloud.Compute, s.Env.Config.Project, s.Env.Config.HomeRegion()
	for _, t := range o.Templates {
		if contains(keep, t) {
			continue
		}
		op, err := c.DeleteInstanceTemplate(ctx, project, region, t)
		if err == nil {
			err = await(ctx, c, project, op)
		}
		if err := ignoreNotFound(err); err != nil {
			return fmt.Errorf("deleting %s: %w", t, err)
		}
	}
	return nil
}

func (s *Replicas) removeGroup(ctx context.Context, g group, obs *ReplicasObserved, o *GroupObserved) error {
	project, region := s.Env.Config.Project, s.Env.Config.HomeRegion()
	x := s.Env.Cloud.ComputeExtras
	steps := []struct {
		on  bool
		do  func() error
		why string
	}{
		{o.Rule, func() error { return x.DeleteForwardingRule(ctx, project, region, g.LB) }, g.LB},
		{o.LB, func() error { return x.DeleteBackendService(ctx, project, region, g.LB) }, g.LB},
		{g.Role == gwtls.RoleControl && obs.Anchor, func() error { return x.DeleteInstanceGroup(ctx, project, obs.AnchorZone, AnchorGroup) }, AnchorGroup},
		{o.MIG && o.MIGOurs, func() error { return x.DeleteInstanceGroupManager(ctx, project, region, g.MIG) }, g.MIG},
		{o.HealthCheck, func() error { return x.DeleteHealthCheck(ctx, project, region, g.HealthCheck) }, g.HealthCheck},
		{true, func() error { return s.deleteTemplates(ctx, o) }, "templates"},
		{o.AddressIP != "", func() error { return x.DeleteAddress(ctx, project, region, g.Address) }, g.Address},
	}
	for _, st := range steps {
		if !st.on {
			continue
		}
		if err := ignoreNotFound(st.do()); err != nil {
			return fmt.Errorf("deleting %s: %w", st.why, err)
		}
	}
	return nil
}

// GatewayParams are what a gateway VM's boot configuration depends on.
type GatewayParams struct {
	Region string
	// AddressSpace is --address-space, which the gateway's container
	// network must stay out of.
	AddressSpace netip.Prefix
}

// GatewayUserData is a gateway-only VM's cloud-init configuration. The
// VM has no service account, so nothing on it has a token. At boot it:
//
//   - makes its key with openssl (there is no image yet: it comes over
//     the mTLS this key is for), which never leaves the VM, and
//     writes the certificate request to the serial console;
//   - waits for the leader to put its certificate and the CA's in
//     silo-gateway-cert and silo-gateway-ca, which only silod can
//     write, and keeps the files current as the leader renews it;
//   - pulls the image through silod's mTLS port, which reads Artifact
//     Registry with silod's credentials for gateways alone (the image is
//     named by digest, so silod can't change it);
//   - runs the gateway, which reaches silod at silo-silod-address over
//     mTLS, with the same DNAT as the control VMs' (gatewayPorts), from
//     any local address, so the NLB's packets keep their source.
func GatewayUserData(p GatewayParams) string {
	net, gwIP := GatewayNetwork(p.AddressSpace)
	r := strings.NewReplacer(
		"@REGISTRY@", p.Region+"-docker.pkg.dev",
		"@GATEWAY_UID@", strconv.Itoa(GatewayUID),
		"@GATEWAY_GID@", strconv.Itoa(GatewayGroup),
		"@GW_NET@", net.String(),
		"@GW_IP@", gwIP.String(),
		"@GW_PORTS@", gatewayPorts(),
		"@METADATA@", MetadataServer,
		"@IMAGE_KEY@", ImageKey,
		"@CERT_KEY@", gwtls.MetadataCert,
		"@CA_KEY@", gwtls.MetadataCA,
		"@SILOD_KEY@", SilodAddressKey,
		"@SILOD_PORT@", strconv.Itoa(gwtls.Port),
		"@SERVER_NAME@", gwtls.ServerName,
	)
	return r.Replace(gatewayUserDataTemplate)
}

const gatewayUserDataTemplate = `#cloud-config
# silo-gateway (silo install, the replicas step). Applied at every boot.
write_files:
- path: /etc/silo/common.sh
  permissions: "0644"
  content: |
    set -euo pipefail
    export HOME=/var/lib/silo
    tls=/var/lib/silo/tls
    mkdir -p "$tls"
    md() { curl -sSf -H 'Metadata-Flavor: Google' "http://@METADATA@/computeMetadata/v1/instance/$1"; }
    silod="$(md attributes/@SILOD_KEY@)"
    grep -q ' @SERVER_NAME@$' /etc/hosts || echo "$silod @SERVER_NAME@" >>/etc/hosts
    # The image comes through silod: this VM has no token to read
    # Artifact Registry with. Docker presents the gateway's certificate.
    certs=/etc/docker/certs.d/@SERVER_NAME@:@SILOD_PORT@
    image() { md attributes/@IMAGE_KEY@ | sed 's|^@REGISTRY@/|@SERVER_NAME@:@SILOD_PORT@/|'; }
- path: /etc/silo/certs.sh
  permissions: "0644"
  content: |
    . /etc/silo/common.sh
    # The key is made here, once, and never leaves the VM; the request
    # goes to the serial console, which only this VM writes and the
    # leader reads.
    if [ ! -s "$tls/key.pem" ]; then
      umask 077
      openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt -out "$tls/key.pem"
    fi
    chown @GATEWAY_UID@:@GATEWAY_GID@ "$tls/key.pem"
    chmod 0400 "$tls/key.pem"
    csr="$(openssl req -new -key "$tls/key.pem" -subj '/CN=silo gateway' | base64 -w0)"
    echo "silo-gateway-csr: $csr" >/dev/ttyS0
    # The certificate, as the leader issues and renews it.
    while true; do
      cert="$(md "attributes/@CERT_KEY@?wait_for_change=true&timeout_sec=600")" || { sleep 5; continue; }
      [ -n "$cert" ] || continue
      md attributes/@CA_KEY@ >"$tls/ca.pem.new" && mv "$tls/ca.pem.new" "$tls/ca.pem"
      printf '%s\n' "$cert" >"$tls/cert.pem.new" && mv "$tls/cert.pem.new" "$tls/cert.pem"
      mkdir -p "$certs"
      cp "$tls/cert.pem" "$certs/client.cert"
      cp "$tls/key.pem" "$certs/client.key"
      cp "$tls/ca.pem" "$certs/ca.crt"
      touch "$tls/ready"
    done
- path: /etc/silo/gateway.sh
  permissions: "0644"
  content: |
    . /etc/silo/common.sh
    until [ -e "$tls/ready" ]; do sleep 2; done
    img="$(image)"
    docker pull "$img" >/dev/null
    docker rm -f silo-gateway silo-gateway-net >/dev/null 2>&1 || true
    docker network inspect silo-gw >/dev/null 2>&1 || docker network create --subnet @GW_NET@ silo-gw >/dev/null
    docker run -d --name silo-gateway-net --network silo-gw --ip @GW_IP@ \
      --user @GATEWAY_UID@:@GATEWAY_GID@ --cap-drop ALL --security-opt no-new-privileges \
      "$img" pause >/dev/null
    iptables -w -t nat -C PREROUTING -m addrtype --dst-type LOCAL -p tcp -m multiport --dports @GW_PORTS@ -j DNAT --to-destination @GW_IP@ 2>/dev/null ||
      iptables -w -t nat -A PREROUTING -m addrtype --dst-type LOCAL -p tcp -m multiport --dports @GW_PORTS@ -j DNAT --to-destination @GW_IP@
    iptables -w -C DOCKER-USER -d @GW_IP@ -p tcp -m multiport --dports @GW_PORTS@ -j ACCEPT 2>/dev/null ||
      iptables -w -I DOCKER-USER -d @GW_IP@ -p tcp -m multiport --dports @GW_PORTS@ -j ACCEPT
    exec docker run --rm --name silo-gateway --network container:silo-gateway-net \
      --user @GATEWAY_UID@:@GATEWAY_GID@ --cap-drop ALL --security-opt no-new-privileges \
      -v "$tls":/tls:ro \
      "$img" gateway --silod-address "$silod:@SILOD_PORT@" \
      --tls-cert /tls/cert.pem --tls-key /tls/key.pem --tls-ca /tls/ca.pem
- path: /etc/silo/watch.sh
  permissions: "0644"
  content: |
    . /etc/silo/common.sh
    cur="$(md attributes/@IMAGE_KEY@)"
    while true; do
      new="$(md "attributes/@IMAGE_KEY@?wait_for_change=true&timeout_sec=600")" || { sleep 10; continue; }
      if [ -n "$new" ] && [ "$new" != "$cur" ]; then
        cur="$new"
        systemctl restart silo-gateway.service
      fi
    done
- path: /etc/systemd/system/silo-certs.service
  permissions: "0644"
  content: |
    [Unit]
    Description=this gateway's key, request and certificate
    Wants=network-online.target
    After=network-online.target
    [Service]
    ExecStart=/bin/bash /etc/silo/certs.sh
    Restart=always
    RestartSec=5
- path: /etc/systemd/system/silo-gateway.service
  permissions: "0644"
  content: |
    [Unit]
    Description=the silo gateway
    Wants=network-online.target
    After=network-online.target docker.service silo-certs.service
    Requires=docker.service
    [Service]
    ExecStart=/bin/bash /etc/silo/gateway.sh
    ExecStopPost=-/usr/bin/docker rm -f silo-gateway silo-gateway-net
    Restart=always
    RestartSec=5
- path: /etc/systemd/system/silo-image-watch.service
  permissions: "0644"
  content: |
    [Unit]
    Description=restart the gateway when silo-image changes
    After=silo-gateway.service
    [Service]
    ExecStart=/bin/bash /etc/silo/watch.sh
    Restart=always
    RestartSec=10
runcmd:
- systemctl daemon-reload
- systemctl start silo-certs.service silo-gateway.service silo-image-watch.service
`
