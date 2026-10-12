package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

// DBEndpointStep is step 11b, db-endpoint (state.md "The control
// subnet" and "Recommended: Private Service Connect", SV-48, SV-49):
// the static internal address silo-db-psc in silo-ctl, the forwarding
// rule silo-db-psc to the instance's PSC service attachment, and the
// private zone silo-db, visible to silo, with an A record from the
// instance's PSC DNS name to that address. silo-ctl is held back from
// every silo's block and the fence's ctl-deny-out denies it to every
// silo, so only the control VMs, in the gateway range, reach it.
//
// Adopting another instance repoints the rule and the zone: the
// address stays, so silod's endpoint keeps its IP.
type DBEndpointStep struct{ Env *Env }

// DBEndpointObserved is the endpoint's three objects and the instance
// they point at.
type DBEndpointObserved struct {
	ComputeEnabled bool `json:"computeEnabled"`
	DNSEnabled     bool `json:"dnsEnabled"`
	// Instance, ServiceAttachment and DNSName are the instance's, once
	// Cloud SQL reports them.
	Instance          string `json:"instance,omitempty"`
	ServiceAttachment string `json:"serviceAttachment,omitempty"`
	DNSName           string `json:"dnsName,omitempty"`

	Address *AddressObserved `json:"address,omitempty"`
	// Rule is the forwarding rule: its description, target and address.
	Rule   *compute.ForwardingRule `json:"rule,omitempty"`
	Zone   *ManagedZone            `json:"zone,omitempty"`
	Record *RecordSet              `json:"record,omitempty"`
}

func (s *DBEndpointStep) Name() string          { return "db-endpoint" }
func (s *DBEndpointStep) homeRegion() string    { return s.Env.Config.HomeRegion() }
func (s *DBEndpointStep) NewObserved() Observed { return &DBEndpointObserved{} }

func (s *DBEndpointStep) networkURL() string {
	return gce.SelfLinkBase + networkPath(s.Env.Config.Project)
}

func (s *DBEndpointStep) Observe(ctx context.Context) (Observed, error) {
	obs := &DBEndpointObserved{}
	project, region := s.Env.Config.Project, s.Env.Config.HomeRegion()
	_, in, _, err := observeDB(ctx, s.Env)
	if err != nil {
		return nil, err
	}
	if in != nil {
		obs.Instance, obs.ServiceAttachment, obs.DNSName = in.Name, in.ServiceAttachment, in.DNSName
	}
	if obs.ComputeEnabled, err = computeOn(ctx, s.Env); err != nil {
		return nil, err
	}
	if obs.ComputeEnabled {
		x := s.Env.Cloud.ComputeExtras
		a, err := x.GetAddress(ctx, project, region, DBEndpoint)
		switch {
		case IsNotFound(err):
		case err != nil:
			return nil, err
		default:
			obs.Address = &AddressObserved{ComputeEnabled: true, Exists: true, Labels: a.Labels, Type: a.AddressType,
				Subnetwork: gce.LastSegment(a.Subnetwork), Address: a.Address}
		}
		r, err := x.GetForwardingRule(ctx, project, region, DBEndpoint)
		switch {
		case IsNotFound(err):
		case err != nil:
			return nil, err
		default:
			obs.Rule = &compute.ForwardingRule{Name: r.Name, Description: r.Description, Target: r.Target,
				IPAddress: r.IPAddress, Network: gce.LastSegment(r.Network)}
		}
	}
	if obs.DNSEnabled, err = s.Env.Enabled(ctx, "dns.googleapis.com"); err != nil || !obs.DNSEnabled {
		return obs, err
	}
	z, err := s.Env.Cloud.DNS.GetManagedZone(ctx, project, DBZone)
	if IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Zone = z
	rs, err := s.Env.Cloud.DNS.GetRecordSet(ctx, project, DBZone, z.DNSName, "A")
	switch {
	case IsNotFound(err):
	case err != nil:
		return nil, err
	default:
		obs.Record = rs
	}
	return obs, nil
}

// Objects of the endpoint, as plan lines name them.
const (
	endpointRule = DBEndpoint + " (forwarding rule)"
	endpointAddr = DBEndpoint + " (address)"
)

func (s *DBEndpointStep) Plan(o Observed) []Change {
	obs := o.(*DBEndpointObserved)
	region := s.Env.Config.HomeRegion()
	target := "the instance's service attachment"
	if obs.ServiceAttachment != "" {
		target = gce.LastSegment(obs.ServiceAttachment) + " (" + obs.Instance + ")"
	}
	dnsName := obs.DNSName
	if dnsName == "" {
		dnsName = "the instance's PSC DNS name"
	}
	var out []Change
	switch a := obs.Address; {
	case a == nil:
		out = append(out, Change{Action: ActionCreate, Object: endpointAddr, Detail: "internal, in " + ControlSubnetName, Estimate: 5 * time.Second})
	case a.Labels[LabelKey] != s.Env.ID:
		return []Change{notOurs(s.Env, DBEndpoint, "silod's database endpoint is install's",
			fmt.Sprintf("gcloud compute addresses delete %s --region %s", DBEndpoint, region))}
	case a.Type != "INTERNAL" || a.Subnetwork != ControlSubnetName:
		return []Change{{Action: ActionFail, Object: endpointAddr,
			Detail: fmt.Sprintf("differs: it is a %s address in %q, not an internal one in %s", a.Type, a.Subnetwork, ControlSubnetName),
			Why:    "the endpoint must be in the control range, which no silo can reach",
			Fix:    fmt.Sprintf("gcloud compute addresses delete %s --region %s", DBEndpoint, region)}}
	}
	switch r := obs.Rule; {
	case r == nil:
		out = append(out, Change{Action: ActionCreate, Object: endpointRule, Detail: "to " + target, Estimate: 20 * time.Second})
	case !strings.HasPrefix(r.Description, DescriptionPrefix+s.Env.ID+" "):
		return []Change{notOurs(s.Env, DBEndpoint, "silod's database endpoint is install's",
			fmt.Sprintf("gcloud compute forwarding-rules delete %s --region %s", DBEndpoint, region))}
	case obs.ServiceAttachment != "" && r.Target != obs.ServiceAttachment:
		out = append(out, Change{Action: ActionUpdate, Object: endpointRule,
			Detail: "repointed to " + target + ", from " + gce.LastSegment(r.Target), Estimate: 30 * time.Second})
	}
	ip := DBEndpoint
	if obs.Address != nil {
		ip = obs.Address.Address
	}
	switch z := obs.Zone; {
	case z == nil:
		out = append(out, Change{Action: ActionCreate, Object: DBZone, Detail: "private zone for " + dnsName + ", A " + ip,
			Estimate: 10 * time.Second})
	case z.Labels[LabelKey] != s.Env.ID:
		return []Change{notOurs(s.Env, DBZone, "the zone is how silod finds its database endpoint", "gcloud dns managed-zones delete "+DBZone)}
	case obs.DNSName != "" && z.DNSName != obs.DNSName:
		out = append(out, Change{Action: ActionUpdate, Object: DBZone,
			Detail: "recreated for " + obs.DNSName + ", not " + z.DNSName, Estimate: 10 * time.Second})
	default:
		visible := false
		for _, n := range z.Networks {
			visible = visible || gce.LastSegment(n) == NetworkName
		}
		switch rec := obs.Record; {
		case !visible || rec == nil || obs.Address != nil && (strings.Join(rec.Rrdatas, " ") != obs.Address.Address || rec.TTL != recordTTL):
			out = append(out, Change{Action: ActionUpdate, Object: DBZone, Detail: "visible to " + NetworkName + ", " + z.DNSName + " A " + ip,
				Estimate: 5 * time.Second})
		}
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Object: DBEndpoint, Detail: fmt.Sprintf("%s in %s, zone %s", ip, ControlSubnetName, DBZone)}}
	}
	return out
}

func (s *DBEndpointStep) PlanRemove(o Observed) []Change {
	obs := o.(*DBEndpointObserved)
	var out []Change
	if z := obs.Zone; z != nil && z.Labels[LabelKey] == s.Env.ID {
		out = append(out, Change{Action: ActionDelete, Object: DBZone, Detail: z.DNSName})
	}
	if r := obs.Rule; r != nil && strings.HasPrefix(r.Description, DescriptionPrefix+s.Env.ID+" ") {
		out = append(out, Change{Action: ActionDelete, Object: endpointRule})
	}
	if a := obs.Address; a != nil && a.Labels[LabelKey] == s.Env.ID {
		out = append(out, Change{Action: ActionDelete, Object: endpointAddr, Detail: a.Address})
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Detail: "no database endpoint"}}
	}
	return out
}

func (s *DBEndpointStep) Apply(ctx context.Context, changes []Change) error {
	x, d := s.Env.Cloud.ComputeExtras, s.Env.Cloud.DNS
	project, region := s.Env.Config.Project, s.Env.Config.HomeRegion()
	// What the endpoint points at: the instance, running (the db step
	// waited for it), with its attachment and DNS name.
	instance := func() (*SQLInstance, error) {
		_, in, _, err := observeDB(ctx, s.Env)
		if err != nil {
			return nil, err
		}
		if in == nil {
			return nil, fmt.Errorf("no database instance: the db-instance step creates it")
		}
		in, err = s.Env.Cloud.SQLAdmin.WaitInstance(ctx, project, in.Name)
		if err != nil {
			return nil, err
		}
		if in.ServiceAttachment == "" || in.DNSName == "" {
			return nil, fmt.Errorf("%s reports no PSC service attachment or DNS name: is PSC on?", in.Name)
		}
		return in, nil
	}
	deleteZone := func() error {
		z, err := d.GetManagedZone(ctx, project, DBZone)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := d.DeleteRecordSet(ctx, project, DBZone, z.DNSName, "A"); err != nil && !IsNotFound(err) {
			return err
		}
		if err := d.DeleteManagedZone(ctx, project, DBZone); err != nil && !IsNotFound(err) {
			return err
		}
		return nil
	}
	ensureZone := func() error {
		in, err := instance()
		if err != nil {
			return err
		}
		a, err := x.GetAddress(ctx, project, region, DBEndpoint)
		if err != nil {
			return fmt.Errorf("reading %s for %s: %w", DBEndpoint, DBZone, err)
		}
		z, err := d.GetManagedZone(ctx, project, DBZone)
		switch {
		case IsNotFound(err):
			err = d.CreateManagedZone(ctx, project, &ManagedZone{Name: DBZone, DNSName: in.DNSName,
				Description: owned(s.Env, "the database's PSC DNS name"), Labels: map[string]string{LabelKey: s.Env.ID},
				Networks: []string{s.networkURL()}})
			if err != nil && !IsAlreadyExists(err) {
				return err
			}
		case err != nil:
			return err
		default:
			visible := false
			for _, n := range z.Networks {
				visible = visible || gce.LastSegment(n) == NetworkName
			}
			if !visible {
				z.Networks = append(z.Networks, s.networkURL())
				if err := d.PatchManagedZone(ctx, project, z); err != nil {
					return err
				}
			}
		}
		rs := &RecordSet{Name: in.DNSName, Type: "A", TTL: recordTTL, Rrdatas: []string{a.Address}}
		if err := d.CreateRecordSet(ctx, project, DBZone, rs); IsAlreadyExists(err) {
			return d.PatchRecordSet(ctx, project, DBZone, rs)
		} else {
			return err
		}
	}
	insertRule := func() error {
		in, err := instance()
		if err != nil {
			return err
		}
		err = x.InsertForwardingRule(ctx, project, region, &compute.ForwardingRule{
			Name:        DBEndpoint,
			Description: owned(s.Env, "the database's PSC endpoint, "+in.Name),
			Network:     s.networkURL(),
			IPAddress:   gce.SelfLinkBase + "projects/" + project + "/regions/" + region + "/addresses/" + DBEndpoint,
			Target:      in.ServiceAttachment,
		})
		if IsAlreadyExists(err) {
			err = nil
		}
		return err
	}
	for _, ch := range changes {
		var err error
		switch {
		case ch.Object == endpointAddr && ch.Action == ActionCreate:
			err = x.InsertAddress(ctx, project, region, &compute.Address{
				Name:        DBEndpoint,
				Description: owned(s.Env, "the database's PSC endpoint"),
				AddressType: "INTERNAL",
				Subnetwork:  subnetPath(project, region, ControlSubnetName),
				Labels:      map[string]string{LabelKey: s.Env.ID},
			})
			if IsAlreadyExists(err) {
				err = nil
			}
		case ch.Object == endpointAddr && ch.Action == ActionDelete:
			if err = x.DeleteAddress(ctx, project, region, DBEndpoint); IsNotFound(err) {
				err = nil
			}
		case ch.Object == endpointRule && ch.Action == ActionCreate:
			err = insertRule()
		case ch.Object == endpointRule && ch.Action == ActionUpdate:
			if err = x.DeleteForwardingRule(ctx, project, region, DBEndpoint); err == nil || IsNotFound(err) {
				err = insertRule()
			}
		case ch.Object == endpointRule && ch.Action == ActionDelete:
			if err = x.DeleteForwardingRule(ctx, project, region, DBEndpoint); IsNotFound(err) {
				err = nil
			}
		case ch.Object == DBZone && ch.Action == ActionCreate:
			err = ensureZone()
		case ch.Object == DBZone && ch.Action == ActionUpdate:
			if strings.HasPrefix(ch.Detail, "recreated") {
				if err = deleteZone(); err != nil {
					return err
				}
			}
			err = ensureZone()
		case ch.Object == DBZone && ch.Action == ActionDelete:
			err = deleteZone()
		}
		if err != nil {
			return err
		}
	}
	return nil
}
