package gwtls

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/store"
	compute "google.golang.org/api/compute/v1"
)

// Gateway is one gateway VM as the issuer sees it.
type Gateway struct {
	Name, Zone string
	// Cert is the certificate in its metadata, nil if none.
	Cert []byte
	// CSR is the last certificate request on its serial console, nil
	// if it hasn't written one yet.
	CSR []byte
}

// Fleet is the gateway VMs: what the issuer reads and the one thing it
// writes. GCEFleet is GCE's.
type Fleet interface {
	Gateways(ctx context.Context) ([]Gateway, error)
	// SetCert puts cert and the CA's certificate in the VM's metadata.
	SetCert(ctx context.Context, zone, name string, cert, ca []byte) error
}

// Issuer is the leader's duty that issues and renews gateway
// certificates.
type Issuer struct {
	Store store.Store
	Fleet Fleet
	// Now is time.Now if nil.
	Now func() time.Time
}

// Run issues a certificate to every gateway VM whose certificate is
// missing, for another key than its last request's, from another CA,
// or within RenewBefore of running out. It returns how many it issued.
func (i *Issuer) Run(ctx context.Context) (int, error) {
	now := time.Now()
	if i.Now != nil {
		now = i.Now()
	}
	ca, err := EnsureCA(ctx, i.Store, now)
	if err != nil {
		return 0, err
	}
	gws, err := i.Fleet.Gateways(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, g := range gws {
		if len(g.CSR) == 0 || !needs(ca, g, now) {
			continue
		}
		cert, err := ca.SignGateway(g.CSR, g.Name, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.Name, err))
			continue
		}
		if err := i.Fleet.SetCert(ctx, g.Zone, g.Name, cert, ca.CertPEM()); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.Name, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

func needs(ca *CA, g Gateway, now time.Time) bool {
	if len(g.Cert) == 0 {
		return true
	}
	c, err := ParseCert(g.Cert)
	if err != nil {
		return true
	}
	if c.CheckSignatureFrom(ca.Cert) != nil || !SamePublicKey(c, g.CSR) {
		return true
	}
	if name, err := GatewayName(c); err != nil || name != g.Name {
		return true
	}
	return !now.Before(c.NotAfter.Add(-RenewBefore))
}

// Every is how often the leader looks for gateways to issue to: a new
// gateway VM waits at most this long at boot.
const Every = 10 * time.Second

// IsGatewayVM reports whether inst can be one of the gateway MIG's VMs.
// The label alone proves nothing: tenants' VMs are in the host project
// too, made through the facade, labels and all. But only install puts
// a VM in the gateway subnet, which the facade never gives a tenant,
// the MIG names its VMs silo-gateway-..., and they have no service
// account.
func IsGatewayVM(inst *compute.Instance) bool {
	if inst.Labels[LabelRole] != RoleGateway || !strings.HasPrefix(inst.Name, GatewayNamePrefix) || len(inst.ServiceAccounts) > 0 {
		return false
	}
	if len(inst.NetworkInterfaces) != 1 {
		return false
	}
	return gce.LastSegment(inst.NetworkInterfaces[0].Subnetwork) == GatewaySubnet
}

// GCEFleet is the gateway MIG's VMs in the home region: the instances
// labeled silo-role=gateway that IsGatewayVM.
type GCEFleet struct {
	GCE             gce.Client
	Project, Region string
}

func (f *GCEFleet) zones(ctx context.Context) ([]string, error) {
	r, err := f.GCE.GetRegion(ctx, f.Project, f.Region)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, z := range r.Zones {
		out = append(out, gce.LastSegment(z))
	}
	return out, nil
}

func (f *GCEFleet) Gateways(ctx context.Context) ([]Gateway, error) {
	zones, err := f.zones(ctx)
	if err != nil {
		return nil, err
	}
	var out []Gateway
	for _, z := range zones {
		list, err := f.GCE.ListInstances(ctx, f.Project, z, gce.ListFilter{Labels: map[string]string{LabelRole: RoleGateway}})
		if err != nil {
			return nil, err
		}
		for _, inst := range list {
			if inst.Status != "RUNNING" || !IsGatewayVM(inst) {
				continue
			}
			g := Gateway{Name: inst.Name, Zone: z}
			if inst.Metadata != nil {
				for _, it := range inst.Metadata.Items {
					if it.Key == MetadataCert && it.Value != nil {
						g.Cert = []byte(*it.Value)
					}
				}
			}
			// The request is written once at boot, early: the first
			// page of the console holds it.
			out1, err := f.GCE.GetSerialPortOutput(ctx, f.Project, z, inst.Name, 1, 0)
			if err != nil {
				log.Printf("gateway certificates: reading %s's console: %v", inst.Name, err)
				continue
			}
			g.CSR = LastCSR(out1.Contents)
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *GCEFleet) SetCert(ctx context.Context, zone, name string, cert, ca []byte) error {
	inst, err := f.GCE.GetInstance(ctx, f.Project, zone, name)
	if err != nil {
		return err
	}
	md := &compute.Metadata{}
	if inst.Metadata != nil {
		md.Fingerprint = inst.Metadata.Fingerprint
		for _, it := range inst.Metadata.Items {
			if it.Key != MetadataCert && it.Key != MetadataCA {
				md.Items = append(md.Items, it)
			}
		}
	}
	c, a := string(cert), string(ca)
	md.Items = append(md.Items, &compute.MetadataItems{Key: MetadataCert, Value: &c}, &compute.MetadataItems{Key: MetadataCA, Value: &a})
	op, err := f.GCE.SetInstanceMetadata(ctx, f.Project, zone, name, md)
	if err != nil {
		return err
	}
	_, err = gce.Await(ctx, f.GCE, f.Project, op)
	return err
}
