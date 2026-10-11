// Package gcp is the real install.Cloud: each of install's small
// interfaces over Google's generated client, with the installer's own
// credentials (--credentials, else Application Default Credentials).
// Those credentials are used only by install; silod never sees them.
package gcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/gceclient"
	"github.com/bwsalmon/silo/pkg/silo/install"
	"golang.org/x/oauth2/google"
	artifactregistry "google.golang.org/api/artifactregistry/v1"
	cloudbilling "google.golang.org/api/cloudbilling/v1"
	crm "google.golang.org/api/cloudresourcemanager/v1"
	computev1 "google.golang.org/api/compute/v1"
	dns "google.golang.org/api/dns/v1"
	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	serviceusage "google.golang.org/api/serviceusage/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// New connects every API install calls. credentials is a
// service-account key file, or "" for Application Default Credentials.
// version goes into the User-Agent.
func New(ctx context.Context, credentials, version string) (install.Cloud, error) {
	var opts []option.ClientOption
	if credentials != "" {
		opts = append(opts, option.WithAuthCredentialsFile(option.ServiceAccount, credentials))
	}
	opts = append(opts, option.WithUserAgent("silo-install/"+version))
	var cloud install.Cloud
	compute, err := gceclient.New(ctx, gceclient.Options{Version: version, ClientOptions: opts})
	if err != nil {
		return cloud, fmt.Errorf("compute: %w", err)
	}
	su, err := serviceusage.NewService(ctx, opts...)
	if err != nil {
		return cloud, fmt.Errorf("serviceusage: %w", err)
	}
	rm, err := crm.NewService(ctx, opts...)
	if err != nil {
		return cloud, fmt.Errorf("cloudresourcemanager: %w", err)
	}
	billing, err := cloudbilling.NewService(ctx, opts...)
	if err != nil {
		return cloud, fmt.Errorf("cloudbilling: %w", err)
	}
	sql, err := sqladmin.NewService(ctx, opts...)
	if err != nil {
		return cloud, fmt.Errorf("sqladmin: %w", err)
	}
	iamSvc, err := iam.NewService(ctx, opts...)
	if err != nil {
		return cloud, fmt.Errorf("iam: %w", err)
	}
	dnsSvc, err := dns.NewService(ctx, opts...)
	if err != nil {
		return cloud, fmt.Errorf("dns: %w", err)
	}
	ar, err := artifactregistry.NewService(ctx, opts...)
	if err != nil {
		return cloud, fmt.Errorf("artifactregistry: %w", err)
	}
	cs, err := computev1.NewService(ctx, opts...)
	if err != nil {
		return cloud, fmt.Errorf("compute: %w", err)
	}
	const scope = "https://www.googleapis.com/auth/cloud-platform"
	var creds *google.Credentials
	if credentials != "" {
		b, rerr := os.ReadFile(credentials)
		if rerr != nil {
			return cloud, rerr
		}
		creds, err = google.CredentialsFromJSON(ctx, b, scope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, scope)
	}
	if err != nil {
		return cloud, fmt.Errorf("credentials for the registry: %w", err)
	}
	return install.Cloud{
		Compute:         compute,
		ServiceUsage:    ServiceUsage{su},
		ResourceManager: ResourceManager{rm},
		Billing:         Billing{billing},
		SQLAdmin:        SQLAdmin{sql},
		IAM:             IAM{iamSvc},
		DNS:             DNS{dnsSvc},
		Artifacts:       ArtifactRegistry{ar},
		ComputeExtras:   ComputeExtras{s: cs, ops: compute},
		Registry:        &Registry{TokenSource: creds.TokenSource},
	}, nil
}

// pollEvery is how often long-running operations are polled.
var pollEvery = 2 * time.Second

func sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pollEvery):
		return nil
	}
}

// ServiceUsage is install.ServiceUsage.
type ServiceUsage struct{ s *serviceusage.Service }

func (c ServiceUsage) ListEnabledServices(ctx context.Context, project string) ([]string, error) {
	var out []string
	err := c.s.Services.List("projects/"+project).Filter("state:ENABLED").Context(ctx).Pages(ctx,
		func(r *serviceusage.ListServicesResponse) error {
			for _, svc := range r.Services {
				if svc.Config != nil {
					out = append(out, svc.Config.Name)
				}
			}
			return nil
		})
	sort.Strings(out)
	return out, err
}

func (c ServiceUsage) EnableServices(ctx context.Context, project string, services []string) error {
	// batchEnable takes at most 20 services.
	for len(services) > 0 {
		n := min(len(services), 20)
		op, err := c.s.Services.BatchEnable("projects/"+project,
			&serviceusage.BatchEnableServicesRequest{ServiceIds: services[:n]}).Context(ctx).Do()
		if err != nil {
			return err
		}
		for !op.Done {
			if err := sleep(ctx); err != nil {
				return err
			}
			if op, err = c.s.Operations.Get(op.Name).Context(ctx).Do(); err != nil {
				return err
			}
		}
		if op.Error != nil {
			return fmt.Errorf("enabling %s: %s", strings.Join(services[:n], ", "), op.Error.Message)
		}
		services = services[n:]
	}
	return nil
}

// ResourceManager is install.ResourceManager.
type ResourceManager struct{ s *crm.Service }

func (c ResourceManager) GetProject(ctx context.Context, project string) (*install.Project, error) {
	p, err := c.s.Projects.Get(project).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := &install.Project{ID: p.ProjectId, Number: p.ProjectNumber}
	if p.Parent != nil && p.Parent.Id != "" {
		switch p.Parent.Type {
		case "organization":
			out.Parent = "organizations/" + p.Parent.Id
		case "folder":
			out.Parent = "folders/" + p.Parent.Id
		default:
			out.Parent = p.Parent.Type + "s/" + p.Parent.Id
		}
	}
	return out, nil
}

func (c ResourceManager) TestIamPermissions(ctx context.Context, project string, permissions []string) ([]string, error) {
	var out []string
	// testIamPermissions takes at most 100 permissions.
	for len(permissions) > 0 {
		n := min(len(permissions), 100)
		r, err := c.s.Projects.TestIamPermissions(project, &crm.TestIamPermissionsRequest{Permissions: permissions[:n]}).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		out = append(out, r.Permissions...)
		permissions = permissions[n:]
	}
	return out, nil
}

func (c ResourceManager) EffectiveOrgPolicy(ctx context.Context, project, constraint string) (*install.OrgPolicy, error) {
	p, err := c.s.Projects.GetEffectiveOrgPolicy("projects/"+project,
		&crm.GetEffectiveOrgPolicyRequest{Constraint: "constraints/" + constraint}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := &install.OrgPolicy{Constraint: constraint}
	if p.BooleanPolicy != nil {
		out.Enforced = p.BooleanPolicy.Enforced
	}
	if l := p.ListPolicy; l != nil {
		out.AllowAll = l.AllValues == "ALLOW"
		out.DenyAll = l.AllValues == "DENY"
		out.Allowed = l.AllowedValues
		out.Denied = l.DeniedValues
	}
	return out, nil
}

// Billing is install.Billing.
type Billing struct{ s *cloudbilling.APIService }

func (c Billing) BillingEnabled(ctx context.Context, project string) (bool, error) {
	info, err := c.s.Projects.GetBillingInfo("projects/" + project).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	return info.BillingEnabled, nil
}

// SQLAdmin is install.SQLAdmin.
type SQLAdmin struct{ s *sqladmin.Service }

func (c SQLAdmin) ListTiers(ctx context.Context, project string) ([]install.SQLTier, error) {
	r, err := c.s.Tiers.List(project).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []install.SQLTier
	for _, t := range r.Items {
		out = append(out, install.SQLTier{Tier: t.Tier, Regions: t.Region})
	}
	return out, nil
}

// ListDatabaseVersions is every version some database flag applies to:
// Cloud SQL Admin has no list of versions as such, and every version
// has flags.
func (c SQLAdmin) ListDatabaseVersions(ctx context.Context, project string) ([]string, error) {
	r, err := c.s.Flags.List().Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range r.Items {
		for _, v := range f.AppliesTo {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (c SQLAdmin) ListInstances(ctx context.Context, project string) ([]install.SQLInstance, error) {
	var out []install.SQLInstance
	err := c.s.Instances.List(project).Context(ctx).Pages(ctx, func(r *sqladmin.InstancesListResponse) error {
		for _, in := range r.Items {
			out = append(out, *fromSQL(in))
		}
		return nil
	})
	return out, err
}

// IAM is install.IAM.
type IAM struct{ s *iam.Service }

func (c IAM) GetServiceAccount(ctx context.Context, project, email string) (*install.ServiceAccount, error) {
	sa, err := c.s.Projects.ServiceAccounts.Get("projects/" + project + "/serviceAccounts/" + email).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return &install.ServiceAccount{Email: sa.Email, DisplayName: sa.DisplayName, Description: sa.Description, Disabled: sa.Disabled}, nil
}

func (c IAM) CreateServiceAccount(ctx context.Context, project, accountID string, sa *install.ServiceAccount) (*install.ServiceAccount, error) {
	got, err := c.s.Projects.ServiceAccounts.Create("projects/"+project, &iam.CreateServiceAccountRequest{
		AccountId:      accountID,
		ServiceAccount: &iam.ServiceAccount{DisplayName: sa.DisplayName, Description: sa.Description},
	}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return &install.ServiceAccount{Email: got.Email, DisplayName: got.DisplayName, Description: got.Description}, nil
}

// DNS is install.DNS.
type DNS struct{ s *dns.Service }

func (c DNS) GetManagedZone(ctx context.Context, project, name string) (*install.ManagedZone, error) {
	z, err := c.s.ManagedZones.Get(project, name).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := &install.ManagedZone{Name: z.Name, DNSName: z.DnsName, Description: z.Description, Labels: z.Labels}
	if z.PrivateVisibilityConfig != nil {
		for _, n := range z.PrivateVisibilityConfig.Networks {
			out.Networks = append(out.Networks, n.NetworkUrl)
		}
	}
	return out, nil
}

func (c DNS) CreateManagedZone(ctx context.Context, project string, z *install.ManagedZone) error {
	mz := &dns.ManagedZone{Name: z.Name, DnsName: z.DNSName, Description: z.Description, Labels: z.Labels}
	if len(z.Networks) > 0 {
		mz.Visibility = "private"
		mz.PrivateVisibilityConfig = &dns.ManagedZonePrivateVisibilityConfig{}
		for _, n := range z.Networks {
			mz.PrivateVisibilityConfig.Networks = append(mz.PrivateVisibilityConfig.Networks,
				&dns.ManagedZonePrivateVisibilityConfigNetwork{NetworkUrl: n})
		}
	}
	_, err := c.s.ManagedZones.Create(project, mz).Context(ctx).Do()
	return err
}

// ArtifactRegistry is install.ArtifactRegistry.
type ArtifactRegistry struct{ s *artifactregistry.Service }

func repoParent(project, location string) string {
	return "projects/" + project + "/locations/" + location
}

func (c ArtifactRegistry) GetRepository(ctx context.Context, project, location, name string) (*install.Repository, error) {
	r, err := c.s.Projects.Locations.Repositories.Get(repoParent(project, location) + "/repositories/" + name).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return &install.Repository{Name: gce.LastSegment(r.Name), Format: r.Format, Labels: r.Labels}, nil
}

func (c ArtifactRegistry) CreateRepository(ctx context.Context, project, location string, r *install.Repository) error {
	op, err := c.s.Projects.Locations.Repositories.Create(repoParent(project, location),
		&artifactregistry.Repository{Format: r.Format, Labels: r.Labels}).RepositoryId(r.Name).Context(ctx).Do()
	if err != nil {
		return err
	}
	for !op.Done {
		if err := sleep(ctx); err != nil {
			return err
		}
		if op, err = c.s.Projects.Locations.Operations.Get(op.Name).Context(ctx).Do(); err != nil {
			return err
		}
	}
	if op.Error != nil {
		return fmt.Errorf("creating repository %s: %s", r.Name, op.Error.Message)
	}
	return nil
}

func (c ArtifactRegistry) DeleteRepository(ctx context.Context, project, location, name string) error {
	op, err := c.s.Projects.Locations.Repositories.Delete(repoParent(project, location) + "/repositories/" + name).Context(ctx).Do()
	if err != nil {
		return err
	}
	for !op.Done {
		if err := sleep(ctx); err != nil {
			return err
		}
		if op, err = c.s.Projects.Locations.Operations.Get(op.Name).Context(ctx).Do(); err != nil {
			return err
		}
	}
	if op.Error != nil {
		return fmt.Errorf("deleting repository %s: %s", name, op.Error.Message)
	}
	return nil
}
