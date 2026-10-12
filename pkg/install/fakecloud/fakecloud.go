// Package fakecloud is an in-memory install.Cloud: the GCE fake
// (gce/fake) for compute, and small fakes of the other APIs install
// calls, for tests of install's steps and of silo install itself.
package fakecloud

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	gcefake "github.com/bwsalmon/silo/pkg/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/install"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// Fake is a project as install sees it. Its fields may be changed
// between calls; every method locks mu.
type Fake struct {
	mu sync.Mutex

	Compute *gcefake.Fake
	Project install.Project
	// Enabled are the enabled services.
	Enabled map[string]bool
	// Denied are permissions testIamPermissions doesn't grant.
	Denied map[string]bool
	// Billing is whether billing is enabled.
	Billing bool
	// Policies are effective org policies by constraint.
	Policies     map[string]*install.OrgPolicy
	Tiers        []install.SQLTier
	Versions     []string
	SQLInstances []install.SQLInstance
	// Databases and Users are each instance's, by instance name.
	Databases map[string][]string
	Users     map[string][]install.SQLUser
	// ProtectionChanges records every change to an instance's deletion
	// protection, as <name>=<on>; FinalBackups are the final backups of
	// deleted instances, by name, with the days each is kept.
	ProtectionChanges []string
	FinalBackups      map[string]int64
	// RefuseRoles makes users.insert refuse databaseRoles, as open
	// question 19 fears it might.
	RefuseRoles bool
	// ForwardingRules are by region/name.
	ForwardingRules map[string]*compute.ForwardingRule
	// HealthChecks, MIGs and BackendServices are by region/name,
	// InstanceGroups by zone/name. A MIG here makes no instances.
	HealthChecks    map[string]*compute.HealthCheck
	MIGs            map[string]*compute.InstanceGroupManager
	BackendServices map[string]*compute.BackendService
	InstanceGroups  map[string]*compute.InstanceGroup
	GroupMembers    map[string][]string
	// MigrateResult, if set, is what silo-migrate reports instead of
	// "ok <the --to its container was given>".
	MigrateResult string

	ServiceAccounts map[string]*install.ServiceAccount
	// Roles are custom roles by id.
	Roles         map[string]*install.Role
	ProjectPolicy *install.Policy
	Zones         map[string]*install.ManagedZone
	// Records are record sets by zone/name/type.
	Records      map[string]*Record
	Repositories map[string]*install.Repository
	// Routers and Addresses are by region/name, InstancePolicies by
	// zone/name.
	Routers          map[string]*compute.Router
	Addresses        map[string]*compute.Address
	InstancePolicies map[string]*install.Policy
	// Images are the digests each repository holds, as
	// <repository>@<digest>. MissingReleases are release tags the
	// release registry doesn't have.
	Images          map[string]bool
	MissingReleases map[string]bool
	// Root is the bootstrap request silod's root was made by, nil
	// before; Members are the root's members.
	Root    *install.BootstrapRequest
	Members []string
	// RootID is the root's id. Silos are the silos below the root, by
	// path, with their ids, and Agents each silo's agents by name, by
	// path. Deleting a silo deletes the GCE objects labelled with its id
	// and its descendants'.
	RootID string
	Silos  map[string]string
	Agents map[string][]string
	// Credentials are the agent credentials silod holds. CredentialAPI
	// is whether silod serves the credentials endpoint; without it, it
	// answers 404, as a silod from before agents.md "Credentials" does.
	Credentials   []string
	CredentialAPI bool
}

// New is a fresh project with no organization: billing on, every
// permission granted, no service enabled, quota for the control plane
// in us-central1 and europe-west4, and silod's release images.
func New(project string) *Fake {
	c := gcefake.New(gcefake.Options{Project: project})
	for _, r := range []string{"us-central1", "europe-west4", "us-east1"} {
		c.SetQuota(r, gce.MetricCPUS, 24)
		c.SetQuota(r, gce.MetricInstances, 100)
		c.SetQuota(r, "STATIC_ADDRESSES", 8)
		c.SetQuota(r, gce.MetricSSDTotalGB, 500)
	}
	c.AddImage("cos-cloud", &compute.Image{Name: "cos-stable-121-18867-90-38", Family: "cos-stable", DiskSizeGb: 10,
		ArchiveSizeBytes: 1 << 30, SelfLink: gce.SelfLinkBase + "projects/cos-cloud/global/images/cos-stable-121-18867-90-38"})
	c.SetQuota("", gce.MetricNetworks, 15)
	c.SetQuota("", gce.MetricSubnetworks, 175)
	c.SetQuota("", gce.MetricFirewalls, 200)
	return &Fake{
		Compute:  c,
		Project:  install.Project{ID: project, Number: 123456789012},
		Enabled:  map[string]bool{},
		Denied:   map[string]bool{},
		Billing:  true,
		Policies: map[string]*install.OrgPolicy{},
		Tiers: []install.SQLTier{
			{Tier: "db-f1-micro", Regions: []string{"us-central1", "europe-west4", "us-east1"}},
			{Tier: "db-g1-small", Regions: []string{"us-central1", "europe-west4", "us-east1"}},
			{Tier: "db-custom-1-3840", Regions: []string{"us-central1", "europe-west4", "us-east1"}},
		},
		Versions:         []string{"MYSQL_8_0", "POSTGRES_15", "POSTGRES_16", "POSTGRES_17"},
		ServiceAccounts:  map[string]*install.ServiceAccount{},
		Roles:            map[string]*install.Role{},
		Zones:            map[string]*install.ManagedZone{},
		Records:          map[string]*Record{},
		Repositories:     map[string]*install.Repository{},
		Routers:          map[string]*compute.Router{},
		Addresses:        map[string]*compute.Address{},
		InstancePolicies: map[string]*install.Policy{},
		Databases:        map[string][]string{},
		Users:            map[string][]install.SQLUser{},
		FinalBackups:     map[string]int64{},
		RootID:           "r2r2r2",
		Silos:            map[string]string{},
		Agents:           map[string][]string{},
		ForwardingRules:  map[string]*compute.ForwardingRule{},
		HealthChecks:     map[string]*compute.HealthCheck{},
		MIGs:             map[string]*compute.InstanceGroupManager{},
		BackendServices:  map[string]*compute.BackendService{},
		InstanceGroups:   map[string]*compute.InstanceGroup{},
		GroupMembers:     map[string][]string{},
		Images:           map[string]bool{},
		MissingReleases:  map[string]bool{},
	}
}

// Cloud is f as an install.Cloud.
func (f *Fake) Cloud() install.Cloud {
	return install.Cloud{
		Compute:         f.Compute,
		ServiceUsage:    serviceUsage{f},
		ResourceManager: resourceManager{f},
		Billing:         billing{f},
		SQLAdmin:        sqlAdmin{f},
		IAM:             iamAPI{f},
		DNS:             dnsAPI{f},
		Artifacts:       artifacts{f},
		ComputeExtras:   computeExtras{f},
		Registry:        registry{f},
		Silod:           silod{f},
	}
}

// EnableAll enables every service the apis step would.
func (f *Fake) EnableAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range install.Services {
		f.Enabled[s] = true
	}
}

// disabled is the error a Google API returns when its service is off.
func disabled(service string) error {
	return &googleapi.Error{Code: http.StatusForbidden, Message: service + " has not been used in this project or it is disabled",
		Errors: []googleapi.ErrorItem{{Reason: "accessNotConfigured"}}}
}

func notFound(what string) error {
	return &googleapi.Error{Code: http.StatusNotFound, Message: what + " not found"}
}

func (f *Fake) need(service string) error {
	if !f.Enabled[service] {
		return disabled(service)
	}
	return nil
}

type serviceUsage struct{ f *Fake }

func (s serviceUsage) ListEnabledServices(ctx context.Context, project string) ([]string, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	var out []string
	for svc, on := range s.f.Enabled {
		if on {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s serviceUsage) EnableServices(ctx context.Context, project string, services []string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	for _, svc := range services {
		s.f.Enabled[svc] = true
	}
	return nil
}

type resourceManager struct{ f *Fake }

func (r resourceManager) GetProject(ctx context.Context, project string) (*install.Project, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	p := r.f.Project
	return &p, nil
}

func (r resourceManager) TestIamPermissions(ctx context.Context, project string, perms []string) ([]string, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	var out []string
	for _, p := range perms {
		// As GCE does: a permission projects don't carry (a billing
		// account's, say) fails the whole request rather than coming
		// back ungranted.
		if strings.HasPrefix(p, "billing.") {
			return nil, fmt.Errorf("googleapi: Error 400: Permission %s is not valid for this resource., badRequest", p)
		}
		if !r.f.Denied[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r resourceManager) EffectiveOrgPolicy(ctx context.Context, project, constraint string) (*install.OrgPolicy, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if p := r.f.Policies[constraint]; p != nil {
		cp := *p
		return &cp, nil
	}
	return &install.OrgPolicy{Constraint: constraint}, nil
}

type billing struct{ f *Fake }

func (b billing) BillingEnabled(ctx context.Context, project string) (bool, error) {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	if err := b.f.need("cloudbilling.googleapis.com"); err != nil {
		return false, err
	}
	return b.f.Billing, nil
}

type sqlAdmin struct{ f *Fake }

func (s sqlAdmin) ListTiers(ctx context.Context, project string) ([]install.SQLTier, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return append([]install.SQLTier(nil), s.f.Tiers...), s.f.need("sqladmin.googleapis.com")
}

func (s sqlAdmin) ListDatabaseVersions(ctx context.Context, project string) ([]string, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return append([]string(nil), s.f.Versions...), s.f.need("sqladmin.googleapis.com")
}

func (s sqlAdmin) ListInstances(ctx context.Context, project string) ([]install.SQLInstance, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return append([]install.SQLInstance(nil), s.f.SQLInstances...), s.f.need("sqladmin.googleapis.com")
}

type iamAPI struct{ f *Fake }

func (a iamAPI) GetServiceAccount(ctx context.Context, project, email string) (*install.ServiceAccount, error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if sa := a.f.ServiceAccounts[email]; sa != nil {
		cp := *sa
		return &cp, nil
	}
	return nil, notFound("service account " + email)
}

func (a iamAPI) CreateServiceAccount(ctx context.Context, project, id string, sa *install.ServiceAccount) (*install.ServiceAccount, error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	email := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", id, project)
	if a.f.ServiceAccounts[email] != nil {
		return nil, &googleapi.Error{Code: http.StatusConflict, Message: "already exists"}
	}
	cp := *sa
	cp.Email = email
	a.f.ServiceAccounts[email] = &cp
	out := cp
	return &out, nil
}

type dnsAPI struct{ f *Fake }

func (d dnsAPI) GetManagedZone(ctx context.Context, project, name string) (*install.ManagedZone, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if z := d.f.Zones[name]; z != nil {
		cp := *z
		return &cp, nil
	}
	return nil, notFound("managed zone " + name)
}

func (d dnsAPI) CreateManagedZone(ctx context.Context, project string, z *install.ManagedZone) error {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if d.f.Zones[z.Name] != nil {
		return &googleapi.Error{Code: http.StatusConflict, Message: "already exists"}
	}
	cp := *z
	d.f.Zones[z.Name] = &cp
	return nil
}

type artifacts struct{ f *Fake }

func (a artifacts) GetRepository(ctx context.Context, project, location, name string) (*install.Repository, error) {
	if location == "" {
		return nil, badRequest("Request contains an invalid argument.")
	}
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if r := a.f.Repositories[location+"/"+name]; r != nil {
		cp := *r
		return &cp, nil
	}
	return nil, notFound("repository " + name)
}

func (a artifacts) CreateRepository(ctx context.Context, project, location string, r *install.Repository) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	key := location + "/" + r.Name
	if a.f.Repositories[key] != nil {
		return &googleapi.Error{Code: http.StatusConflict, Message: "already exists"}
	}
	cp := *r
	a.f.Repositories[key] = &cp
	return nil
}

func (a artifacts) DeleteRepository(ctx context.Context, project, location, name string) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	key := location + "/" + name
	if a.f.Repositories[key] == nil {
		return notFound("repository " + name)
	}
	delete(a.f.Repositories, key)
	prefix := location + "-docker.pkg.dev/" + project + "/" + name + "/"
	for img := range a.f.Images {
		if strings.HasPrefix(img, prefix) {
			delete(a.f.Images, img)
		}
	}
	return nil
}
