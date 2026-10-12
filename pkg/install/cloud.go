package install

import (
	"context"
	"errors"
	"net/http"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// Cloud is every GCP API install calls, each behind a small interface
// so that every step can be tested against a fake or a recorded
// observation. Package install/gcp has the real ones. A step that needs
// another API adds its interface here.
type Cloud struct {
	Compute         gce.Client
	ServiceUsage    ServiceUsage
	ResourceManager ResourceManager
	Billing         Billing
	SQLAdmin        SQLAdmin
	IAM             IAM
	DNS             DNS
	Artifacts       ArtifactRegistry
	ComputeExtras   ComputeExtras
	// Registry copies the silod image into the project (step 11).
	Registry Registry
	// Silod is silod's own API through the front door, for steps 13 to
	// 15. Nil until the control VM exists to reach it on.
	Silod Silod
	// Cloud Quotas is Compute's ListComputeQuotaInfos (gce.Quotas).
}

// ServiceUsage is serviceusage.googleapis.com.
type ServiceUsage interface {
	// ListEnabledServices lists the project's enabled services, as
	// compute.googleapis.com.
	ListEnabledServices(ctx context.Context, project string) ([]string, error)
	// EnableServices enables services and waits for it. Enabling an
	// enabled service is not an error.
	EnableServices(ctx context.Context, project string, services []string) error
}

// Project is the part of Resource Manager's project install reads.
type Project struct {
	ID     string `json:"id"`
	Number int64  `json:"number,omitempty"`
	// Parent is organizations/<id> or folders/<id>, or empty for a
	// project with no organization.
	Parent string `json:"parent,omitempty"`
}

// OrgPolicy is a project's effective policy for one constraint.
type OrgPolicy struct {
	Constraint string `json:"constraint"`
	// Enforced is a boolean constraint's value.
	Enforced bool `json:"enforced,omitempty"`
	// AllowAll and DenyAll are a list constraint's allValues.
	AllowAll bool     `json:"allowAll,omitempty"`
	DenyAll  bool     `json:"denyAll,omitempty"`
	Allowed  []string `json:"allowed,omitempty"`
	Denied   []string `json:"denied,omitempty"`
}

// Restricts reports whether a list policy limits its values at all.
func (p *OrgPolicy) Restricts() bool {
	return p != nil && !p.AllowAll && (p.DenyAll || len(p.Allowed) > 0 || len(p.Denied) > 0)
}

// Permits reports whether a list policy allows value, given the
// values it may be matched by (a region and its value groups, say).
func (p *OrgPolicy) Permits(values ...string) bool {
	if p == nil || p.AllowAll {
		return true
	}
	if p.DenyAll {
		return false
	}
	for _, v := range values {
		for _, d := range p.Denied {
			if d == v {
				return false
			}
		}
	}
	if len(p.Allowed) == 0 {
		return true
	}
	for _, v := range values {
		for _, a := range p.Allowed {
			if a == v {
				return true
			}
		}
	}
	return false
}

// ResourceManager is cloudresourcemanager.googleapis.com.
type ResourceManager interface {
	GetProject(ctx context.Context, project string) (*Project, error)
	// TestIamPermissions returns the subset of permissions the caller
	// holds on the project.
	TestIamPermissions(ctx context.Context, project string, permissions []string) ([]string, error)
	// EffectiveOrgPolicy is the project's effective policy for
	// constraint (without its constraints/ prefix).
	EffectiveOrgPolicy(ctx context.Context, project, constraint string) (*OrgPolicy, error)
	// GetIamPolicy and SetIamPolicy are the project's IAM policy, at
	// version 3 so that conditional bindings survive the round trip.
	// SetIamPolicy fails with 409 if the policy's etag is stale.
	GetIamPolicy(ctx context.Context, project string) (*Policy, error)
	SetIamPolicy(ctx context.Context, project string, p *Policy) error
}

// Billing is cloudbilling.googleapis.com, read only.
type Billing interface {
	BillingEnabled(ctx context.Context, project string) (bool, error)
}

// SQLTier is a Cloud SQL machine tier and the regions offering it.
type SQLTier struct {
	Tier    string   `json:"tier"`
	Regions []string `json:"regions,omitempty"`
}

// SQLInstance is the part of a Cloud SQL instance install reads and
// sets (state.md "The Cloud SQL instance", SV-47).
type SQLInstance struct {
	Name            string            `json:"name"`
	Region          string            `json:"region,omitempty"`
	DatabaseVersion string            `json:"databaseVersion,omitempty"`
	State           string            `json:"state,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`

	Edition string `json:"edition,omitempty"`
	Tier    string `json:"tier,omitempty"`
	// Availability is ZONAL or REGIONAL.
	Availability string `json:"availability,omitempty"`
	DiskType     string `json:"diskType,omitempty"`
	DiskSizeGB   int64  `json:"diskSizeGB,omitempty"`
	AutoResize   bool   `json:"autoResize,omitempty"`
	Backups      bool   `json:"backups,omitempty"`
	// BackupRetention is how many automated backups are kept.
	BackupRetention int64 `json:"backupRetention,omitempty"`
	PITR            bool  `json:"pitr,omitempty"`
	// LogRetentionDays is how many days of transaction logs PITR keeps.
	LogRetentionDays   int64 `json:"logRetentionDays,omitempty"`
	DeletionProtection bool  `json:"deletionProtection,omitempty"`
	PublicIP           bool  `json:"publicIP,omitempty"`
	// PrivateNetwork is the VPC of private services access, if any.
	PrivateNetwork string `json:"privateNetwork,omitempty"`
	PSC            bool   `json:"psc,omitempty"`
	// PSCConsumers are the projects allowed to connect over PSC.
	PSCConsumers []string          `json:"pscConsumers,omitempty"`
	Flags        map[string]string `json:"flags,omitempty"`
	// MaintenanceDay is 1 (Monday) to 7, MaintenanceHour 0 to 23, UTC.
	MaintenanceDay  int64 `json:"maintenanceDay,omitempty"`
	MaintenanceHour int64 `json:"maintenanceHour,omitempty"`

	// Read only: the instance's PSC service attachment, its PSC DNS
	// name, and its connection name, <project>:<region>:<name>.
	ServiceAttachment string `json:"serviceAttachment,omitempty"`
	DNSName           string `json:"dnsName,omitempty"`
	ConnectionName    string `json:"connectionName,omitempty"`
}

// SQLUser is a Cloud SQL database user.
type SQLUser struct {
	Name string `json:"name"`
	// Type is BUILT_IN, CLOUD_IAM_USER, CLOUD_IAM_SERVICE_ACCOUNT, ...
	Type string `json:"type,omitempty"`
	// DatabaseRoles are Postgres roles the Admin API grants the user
	// when it creates it.
	DatabaseRoles []string `json:"databaseRoles,omitempty"`
}

// SQLAdmin is sqladmin.googleapis.com.
type SQLAdmin interface {
	ListTiers(ctx context.Context, project string) ([]SQLTier, error)
	// ListDatabaseVersions lists the database versions Cloud SQL offers.
	ListDatabaseVersions(ctx context.Context, project string) ([]string, error)
	ListInstances(ctx context.Context, project string) ([]SQLInstance, error)
	GetInstance(ctx context.Context, project, name string) (*SQLInstance, error)
	// InsertInstance starts creating the instance and returns without
	// waiting for it: creation takes minutes.
	InsertInstance(ctx context.Context, project string, in *SQLInstance) error
	// PatchInstance sets every setting install owns, the version and
	// the labels to in's, and waits for it.
	PatchInstance(ctx context.Context, project string, in *SQLInstance) error
	// WaitInstance waits until the instance is RUNNABLE with no
	// operation running on it, and returns it.
	WaitInstance(ctx context.Context, project, name string) (*SQLInstance, error)
	ListDatabases(ctx context.Context, project, instance string) ([]string, error)
	// InsertDatabase creates a database and waits for it.
	InsertDatabase(ctx context.Context, project, instance, name string) error
	ListUsers(ctx context.Context, project, instance string) ([]SQLUser, error)
	// InsertUser creates a user, with no password, and waits for it.
	InsertUser(ctx context.Context, project, instance string, u *SQLUser) error
	// SetDeletionProtection sets the instance's deletion protection, and
	// nothing else, and waits for it. Only uninstall --delete-data
	// calls it, to lift it on the one instance it is about to delete.
	SetDeletionProtection(ctx context.Context, project, name string, on bool) error
	// DeleteInstance deletes the instance, taking a final backup that
	// Cloud SQL keeps for finalBackupDays days, and waits for it.
	DeleteInstance(ctx context.Context, project, name string, finalBackupDays int64) error
}

// ServiceAccount is the part of an IAM service account install reads.
type ServiceAccount struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

// Role is the part of an IAM custom role install reads.
type Role struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	Stage       string   `json:"stage,omitempty"`
	// Deleted is a role deleted within the last 7 days, whose id can't
	// be used again until it is undeleted.
	Deleted bool `json:"deleted,omitempty"`
}

// IAM is iam.googleapis.com, for the service-account and role steps.
type IAM interface {
	GetServiceAccount(ctx context.Context, project, email string) (*ServiceAccount, error)
	CreateServiceAccount(ctx context.Context, project, accountID string, sa *ServiceAccount) (*ServiceAccount, error)
	// PatchServiceAccount sets the account's display name and
	// description.
	PatchServiceAccount(ctx context.Context, project string, sa *ServiceAccount) error
	EnableServiceAccount(ctx context.Context, project, email string) error
	DeleteServiceAccount(ctx context.Context, project, email string) error
	// GetRole reads projects/<project>/roles/<id>, deleted or not.
	GetRole(ctx context.Context, project, id string) (*Role, error)
	CreateRole(ctx context.Context, project, id string, r *Role) error
	// UpdateRole sets the role's title, description, permissions and
	// stage, undeleting it first if it is deleted.
	UpdateRole(ctx context.Context, project, id string, r *Role) error
	DeleteRole(ctx context.Context, project, id string) error
}

// Policy is an IAM policy, as getIamPolicy returns it and setIamPolicy
// takes it back: a read-modify-write guarded by Etag.
type Policy struct {
	Version  int64      `json:"version,omitempty"`
	Etag     string     `json:"etag,omitempty"`
	Bindings []*Binding `json:"bindings,omitempty"`
}

// Binding is one of a policy's bindings. Install reads and writes only
// unconditional ones, and carries conditional ones through unchanged.
type Binding struct {
	Role      string     `json:"role"`
	Members   []string   `json:"members"`
	Condition *Condition `json:"condition,omitempty"`
}

// Condition is a binding's IAM condition.
type Condition struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Expression  string `json:"expression"`
}

// Has reports whether p grants role to member unconditionally.
func (p *Policy) Has(role, member string) bool {
	if p == nil {
		return false
	}
	for _, b := range p.Bindings {
		if b.Role == role && b.Condition == nil {
			for _, m := range b.Members {
				if m == member {
					return true
				}
			}
		}
	}
	return false
}

// Grant adds an unconditional grant of role to member. It reports
// whether p changed.
func (p *Policy) Grant(role, member string) bool {
	if p.Has(role, member) {
		return false
	}
	for _, b := range p.Bindings {
		if b.Role == role && b.Condition == nil {
			b.Members = append(b.Members, member)
			return true
		}
	}
	p.Bindings = append(p.Bindings, &Binding{Role: role, Members: []string{member}})
	return true
}

// Revoke removes member's unconditional grant of role, and a binding it
// leaves empty. It reports whether p changed.
func (p *Policy) Revoke(role, member string) bool {
	changed := false
	var out []*Binding
	for _, b := range p.Bindings {
		if b.Role == role && b.Condition == nil {
			var keep []string
			for _, m := range b.Members {
				if m == member {
					changed = true
				} else {
					keep = append(keep, m)
				}
			}
			if len(keep) == 0 {
				continue
			}
			b.Members = keep
		}
		out = append(out, b)
	}
	p.Bindings = out
	return changed
}

// ManagedZone is the part of a Cloud DNS zone install reads.
type ManagedZone struct {
	Name        string            `json:"name"`
	DNSName     string            `json:"dnsName"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	// Networks are the network URLs a private zone is visible to.
	Networks []string `json:"networks,omitempty"`
}

// RecordSet is a DNS record set.
type RecordSet struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int64    `json:"ttl,omitempty"`
	Rrdatas []string `json:"rrdatas,omitempty"`
}

// DNS is dns.googleapis.com, for the dns and db-endpoint steps.
type DNS interface {
	GetManagedZone(ctx context.Context, project, name string) (*ManagedZone, error)
	CreateManagedZone(ctx context.Context, project string, z *ManagedZone) error
	// PatchManagedZone sets the private zone's networks.
	PatchManagedZone(ctx context.Context, project string, z *ManagedZone) error
	DeleteManagedZone(ctx context.Context, project, name string) error
	GetRecordSet(ctx context.Context, project, zone, name, typ string) (*RecordSet, error)
	CreateRecordSet(ctx context.Context, project, zone string, rs *RecordSet) error
	// PatchRecordSet replaces the record set's ttl and rrdatas.
	PatchRecordSet(ctx context.Context, project, zone string, rs *RecordSet) error
	DeleteRecordSet(ctx context.Context, project, zone, name, typ string) error
}

// ComputeExtras are the Compute calls install makes that silod doesn't,
// so that gce.Client, silod's, stays what silod calls: Cloud Router and
// NAT, addresses, Private Google Access and an instance's IAM policy.
// Each waits for its operation.
type ComputeExtras interface {
	SetPrivateIpGoogleAccess(ctx context.Context, project, region, subnet string, on bool) error
	GetRouter(ctx context.Context, project, region, name string) (*compute.Router, error)
	InsertRouter(ctx context.Context, project, region string, r *compute.Router) error
	// PatchRouter sets the router's NATs.
	PatchRouter(ctx context.Context, project, region string, r *compute.Router) error
	DeleteRouter(ctx context.Context, project, region, name string) error
	GetAddress(ctx context.Context, project, region, name string) (*compute.Address, error)
	InsertAddress(ctx context.Context, project, region string, a *compute.Address) error
	DeleteAddress(ctx context.Context, project, region, name string) error
	// InstanceZone is the zone of region holding the instance name, or
	// "" if none does.
	InstanceZone(ctx context.Context, project, region, name string) (string, error)
	GetInstanceIamPolicy(ctx context.Context, project, zone, name string) (*Policy, error)
	SetInstanceIamPolicy(ctx context.Context, project, zone, name string, p *Policy) error
	GetForwardingRule(ctx context.Context, project, region, name string) (*compute.ForwardingRule, error)
	InsertForwardingRule(ctx context.Context, project, region string, r *compute.ForwardingRule) error
	DeleteForwardingRule(ctx context.Context, project, region, name string) error
	// GuestAttribute reads the VM's guest attribute key
	// (namespace/name); a 404 means it isn't set yet.
	GuestAttribute(ctx context.Context, project, zone, instance, key string) (string, error)

	// The replica MIGs and their internal passthrough NLBs (the
	// replicas step): regional health checks, regional MIGs, regional
	// backend services, and the zonal unmanaged group that puts
	// silo-control behind the control NLB. Insert, Patch and Delete
	// wait for their operations.
	GetHealthCheck(ctx context.Context, project, region, name string) (*compute.HealthCheck, error)
	InsertHealthCheck(ctx context.Context, project, region string, hc *compute.HealthCheck) error
	DeleteHealthCheck(ctx context.Context, project, region, name string) error
	GetInstanceGroupManager(ctx context.Context, project, region, name string) (*compute.InstanceGroupManager, error)
	InsertInstanceGroupManager(ctx context.Context, project, region string, m *compute.InstanceGroupManager) error
	// PatchInstanceGroupManager sets the MIG's template, target size and
	// update policy to m's.
	PatchInstanceGroupManager(ctx context.Context, project, region string, m *compute.InstanceGroupManager) error
	DeleteInstanceGroupManager(ctx context.Context, project, region, name string) error
	GetBackendService(ctx context.Context, project, region, name string) (*compute.BackendService, error)
	InsertBackendService(ctx context.Context, project, region string, b *compute.BackendService) error
	// PatchBackendService sets the backend service's backends to b's.
	PatchBackendService(ctx context.Context, project, region string, b *compute.BackendService) error
	DeleteBackendService(ctx context.Context, project, region, name string) error
	GetInstanceGroup(ctx context.Context, project, zone, name string) (*compute.InstanceGroup, error)
	InsertInstanceGroup(ctx context.Context, project, zone string, g *compute.InstanceGroup) error
	// GroupInstances lists an unmanaged group's instances by name;
	// AddGroupInstances adds instances to it by name.
	GroupInstances(ctx context.Context, project, zone, name string) ([]string, error)
	AddGroupInstances(ctx context.Context, project, zone, name string, instances []string) error
	DeleteInstanceGroup(ctx context.Context, project, zone, name string) error
}

// Repository is the part of an Artifact Registry repository install
// reads.
type Repository struct {
	Name   string            `json:"name"`
	Format string            `json:"format"`
	Labels map[string]string `json:"labels,omitempty"`
}

// ArtifactRegistry is artifactregistry.googleapis.com, for the image
// step.
type ArtifactRegistry interface {
	GetRepository(ctx context.Context, project, location, name string) (*Repository, error)
	CreateRepository(ctx context.Context, project, location string, r *Repository) error
	// DeleteRepository deletes the repository and every image in it.
	DeleteRepository(ctx context.Context, project, location, name string) error
}

// Registry is the container registries the image step reads and
// writes, over the OCI distribution API with the installer's
// credentials. A repository is <host>/<path>, without a tag or digest.
type Registry interface {
	// Resolve is the manifest digest (sha256:...) that ref, a
	// repository with a :tag or @digest, names.
	Resolve(ctx context.Context, ref string) (string, error)
	// Has reports whether repository holds the manifest digest.
	Has(ctx context.Context, repository, digest string) (bool, error)
	// Copy copies the manifest src@digest and everything it refers to
	// (an index's manifests, their configs and layers) into dst, by
	// digest. Copying what is already there is not an error.
	Copy(ctx context.Context, src, digest, dst string) error
}

// Silod is silod's API through the front door: an IAP tunnel to the
// control VM's port 8443, with an ID token for the installer's own
// identity on every call (gateway.md, "Front door", SV-88).
type Silod interface {
	// Health reports whether silod is serving and whether the root
	// exists. Any verified caller may ask, member or not.
	Health(ctx context.Context) (SilodHealth, error)
	// Bootstrap creates the root. silod accepts it only while there is
	// no root.
	Bootstrap(ctx context.Context, req *BootstrapRequest) error
	// Root is describe / (the root silo, as its members see it).
	Root(ctx context.Context) (*RootSilo, error)
	// Members are the identities mapped to silo, by path.
	Members(ctx context.Context, silo string) ([]string, error)
	// PutMember maps identity to silo.
	PutMember(ctx context.Context, silo, identity string) error
	// Regions is the facade's regions.list, as the installer's silo sees
	// it.
	Regions(ctx context.Context) ([]string, error)

	// Uninstall's (install.md "silo uninstall", SV-96).
	//
	// Tree is describe / with its subtree: every silo silod knows, the
	// root first.
	Tree(ctx context.Context) ([]SiloNode, error)
	// Agents are the agents in silo, by name.
	Agents(ctx context.Context, silo string) ([]string, error)
	// DeleteSilo is silo delete --force on path: the silo, its subtree
	// and every object in them. It waits for the delete to finish.
	DeleteSilo(ctx context.Context, path string) error
	// DeleteAgent deletes the agent silo/name.
	DeleteAgent(ctx context.Context, silo, name string) error
	// Credentials are the agent credentials silod holds, by name; never
	// their secrets. ErrNoCredentialAPI is a silod with no credentials
	// endpoint (one from before agents.md "Credentials"), which holds
	// none.
	Credentials(ctx context.Context) ([]string, error)
	// DeleteCredential deletes one agent credential from the database.
	DeleteCredential(ctx context.Context, name string) error
}

// ErrNoCredentialAPI is Silod.Credentials's answer from a silod with no
// credentials endpoint: it holds no credentials.
var ErrNoCredentialAPI = errors.New("silod has no credentials endpoint")

// SiloNode is one silo of the tree, as uninstall lists it.
type SiloNode struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// SilodHealth is /silod/health's answer.
type SilodHealth struct {
	Status string `json:"status"`
	Root   bool   `json:"root"`
}

// BootstrapRequest is silod's /silod/bootstrap body (fenceapi's
// BootstrapRequest, which install doesn't import so that the CLI carries
// no store).
type BootstrapRequest struct {
	Name           string                       `json:"name,omitempty"`
	AddressSpace   string                       `json:"address_space"`
	GatewayRange   string                       `json:"gateway_range"`
	ControlRange   string                       `json:"control_range"`
	Regions        []string                     `json:"regions"`
	Capacity       map[string]map[string]string `json:"capacity"`
	ModelBudgetUSD string                       `json:"model_budget_usd,omitempty"`
	BudgetPeriod   string                       `json:"budget_period,omitempty"`
	Egress         *RootEgress                  `json:"egress"`
	GrantPolicy    map[string]string            `json:"grant_policy"`
	AgentPolicy    map[string]any               `json:"agent_policy,omitempty"`
	Admins         []string                     `json:"admins"`
	Labels         map[string]string            `json:"labels,omitempty"`
}

// RootEgress is the root's egress policy.
type RootEgress struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// RootSilo is the part of describe / that verify prints.
type RootSilo struct {
	Path    string   `json:"path"`
	Regions []string `json:"regions"`
	Blocks  []string `json:"blocks"`
	// Quota is region to class to granted amount.
	Quota map[string]map[string]string `json:"quota"`
}

// IsNotFound reports whether err is a Google API's 404.
func IsNotFound(err error) bool { return code(err) == http.StatusNotFound }

// IsConflict reports whether err is a Google API's 409 or 412: a
// read-modify-write whose etag or fingerprint went stale.
func IsConflict(err error) bool {
	c := code(err)
	return c == http.StatusConflict || c == http.StatusPreconditionFailed
}

// IsAlreadyExists reports whether err is a Google API's 409 on a create.
func IsAlreadyExists(err error) bool { return code(err) == http.StatusConflict }

// IsServiceDisabled reports whether err is a Google API refusing
// because its service isn't enabled on the project.
func IsServiceDisabled(err error) bool {
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusForbidden {
		return false
	}
	for _, it := range ge.Errors {
		if it.Reason == "accessNotConfigured" || it.Reason == "SERVICE_DISABLED" {
			return true
		}
	}
	for _, d := range ge.Details {
		if m, ok := d.(map[string]any); ok && m["reason"] == "SERVICE_DISABLED" {
			return true
		}
	}
	return false
}

func code(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}
