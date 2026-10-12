package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
)

// The private zone silo.internal lives in.
const (
	ZoneName    = "silo-internal"
	ZoneDNSName = "silo.internal."
	recordTTL   = 300
)

// DNSStep is step 10, dns: the private zone silo-internal for
// silo.internal., visible to silo, with silo.internal. pointing at
// silo-gateway-ip, or, once the replicas step has made one, at the
// gateway NLB's address, else the control NLB's (FrontAddress).
type DNSStep struct{ Env *Env }

// DNSObserved is the zone and its A record, and the gateway address the
// record should hold ("" if there is none yet).
type DNSObserved struct {
	DNSEnabled bool         `json:"dnsEnabled"`
	Zone       *ManagedZone `json:"zone,omitempty"`
	Record     *RecordSet   `json:"record,omitempty"`
	GatewayIP  string       `json:"gatewayIP,omitempty"`
}

func (s *DNSStep) Name() string          { return "dns" }
func (s *DNSStep) homeRegion() string    { return s.Env.Config.HomeRegion() }
func (s *DNSStep) NewObserved() Observed { return &DNSObserved{} }

func (s *DNSStep) networkURL() string { return gce.SelfLinkBase + networkPath(s.Env.Config.Project) }

func (s *DNSStep) Observe(ctx context.Context) (Observed, error) {
	obs := &DNSObserved{}
	project := s.Env.Config.Project
	on, err := computeOn(ctx, s.Env)
	if err != nil {
		return nil, err
	}
	if on {
		if obs.GatewayIP, err = frontIP(ctx, s.Env); err != nil {
			return nil, err
		}
	}
	if obs.DNSEnabled, err = s.Env.Enabled(ctx, "dns.googleapis.com"); err != nil || !obs.DNSEnabled {
		return obs, err
	}
	z, err := s.Env.Cloud.DNS.GetManagedZone(ctx, project, ZoneName)
	if IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Zone = z
	rs, err := s.Env.Cloud.DNS.GetRecordSet(ctx, project, ZoneName, ZoneDNSName, "A")
	switch {
	case IsNotFound(err):
	case err != nil:
		return nil, err
	default:
		obs.Record = rs
	}
	return obs, nil
}

func (s *DNSStep) Plan(o Observed) []Change {
	obs := o.(*DNSObserved)
	target := obs.GatewayIP
	if target == "" {
		target = GatewayAddress
	}
	record := Change{Action: ActionCreate, Object: ZoneDNSName, Detail: "A " + target, Estimate: 5 * time.Second}
	if obs.Zone == nil {
		return []Change{{Action: ActionCreate, Object: ZoneName, Detail: "private zone for " + ZoneDNSName + ", visible to " + NetworkName,
			Estimate: 5 * time.Second}, record}
	}
	z := obs.Zone
	if z.Labels[LabelKey] != s.Env.ID {
		return []Change{notOurs(s.Env, ZoneName, "silo.internal is how silos and the control VM find the gateway",
			"gcloud dns managed-zones delete "+ZoneName)}
	}
	if z.DNSName != ZoneDNSName {
		return []Change{{Action: ActionFail, Object: ZoneName, Detail: "differs: it is for " + z.DNSName,
			Why: "a zone's DNS name can't change in place", Fix: "gcloud dns managed-zones delete " + ZoneName + ", then install again"}}
	}
	var out []Change
	visible := false
	for _, n := range z.Networks {
		visible = visible || gce.LastSegment(n) == NetworkName
	}
	if !visible {
		out = append(out, Change{Action: ActionUpdate, Object: ZoneName, Detail: "visible to " + NetworkName, Estimate: 5 * time.Second})
	}
	switch r := obs.Record; {
	case r == nil:
		out = append(out, record)
	case obs.GatewayIP != "" && (strings.Join(r.Rrdatas, " ") != obs.GatewayIP || r.TTL != recordTTL):
		out = append(out, Change{Action: ActionUpdate, Object: ZoneDNSName,
			Detail: fmt.Sprintf("A %s, not %s", obs.GatewayIP, strings.Join(r.Rrdatas, " ")), Estimate: 5 * time.Second})
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Object: ZoneName, Detail: ZoneDNSName + " A " + target}}
	}
	return out
}

func (s *DNSStep) PlanRemove(o Observed) []Change {
	obs := o.(*DNSObserved)
	if obs.Zone == nil {
		return []Change{{Action: ActionOK, Detail: "no zone " + ZoneName}}
	}
	if obs.Zone.Labels[LabelKey] != s.Env.ID {
		return []Change{{Action: ActionOK, Object: ZoneName, Detail: "is not this install's; left alone"}}
	}
	var out []Change
	if obs.Record != nil {
		out = append(out, Change{Action: ActionDelete, Object: ZoneDNSName, Detail: "A " + strings.Join(obs.Record.Rrdatas, " ")})
	}
	return append(out, Change{Action: ActionDelete, Object: ZoneName})
}

func (s *DNSStep) Apply(ctx context.Context, changes []Change) error {
	d := s.Env.Cloud.DNS
	project := s.Env.Config.Project
	record := func() (*RecordSet, error) {
		ip, err := frontIP(ctx, s.Env)
		if err != nil || ip == "" {
			return nil, fmt.Errorf("reading %s for %s: %v", GatewayAddress, ZoneDNSName, err)
		}
		return &RecordSet{Name: ZoneDNSName, Type: "A", TTL: recordTTL, Rrdatas: []string{ip}}, nil
	}
	for _, ch := range changes {
		var err error
		switch {
		case ch.Object == ZoneName && ch.Action == ActionCreate:
			err = d.CreateManagedZone(ctx, project, &ManagedZone{Name: ZoneName, DNSName: ZoneDNSName,
				Description: owned(s.Env, "silo.internal, the gateway"), Labels: map[string]string{LabelKey: s.Env.ID},
				Networks: []string{s.networkURL()}})
			if IsAlreadyExists(err) {
				err = nil
			}
		case ch.Object == ZoneName && ch.Action == ActionUpdate:
			var z *ManagedZone
			if z, err = d.GetManagedZone(ctx, project, ZoneName); err == nil {
				z.Networks = append(z.Networks, s.networkURL())
				err = d.PatchManagedZone(ctx, project, z)
			}
		case ch.Object == ZoneName && ch.Action == ActionDelete:
			if err = d.DeleteManagedZone(ctx, project, ZoneName); IsNotFound(err) {
				err = nil
			}
		case ch.Action == ActionCreate:
			var rs *RecordSet
			if rs, err = record(); err == nil {
				if err = d.CreateRecordSet(ctx, project, ZoneName, rs); IsAlreadyExists(err) {
					err = d.PatchRecordSet(ctx, project, ZoneName, rs)
				}
			}
		case ch.Action == ActionUpdate:
			var rs *RecordSet
			if rs, err = record(); err == nil {
				err = d.PatchRecordSet(ctx, project, ZoneName, rs)
			}
		case ch.Action == ActionDelete:
			if err = d.DeleteRecordSet(ctx, project, ZoneName, ZoneDNSName, "A"); IsNotFound(err) {
				err = nil
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// frontIP is the address silo.internal names as the project is now
// (FrontAddress): "" before silo-gateway-ip exists.
func frontIP(ctx context.Context, env *Env) (string, error) {
	x, project, region := env.Cloud.ComputeExtras, env.Config.Project, env.Config.HomeRegion()
	var ips [3]string
	for i, name := range []string{gatewayGroup.LB, controlGroup.LB} {
		r, err := x.GetForwardingRule(ctx, project, region, name)
		switch {
		case IsNotFound(err):
		case err != nil:
			return "", err
		default:
			ips[i] = r.IPAddress
		}
	}
	a, err := x.GetAddress(ctx, project, region, GatewayAddress)
	switch {
	case IsNotFound(err):
	case err != nil:
		return "", err
	default:
		ips[2] = a.Address
	}
	return FrontAddress(ips[0], ips[1], ips[2]), nil
}
