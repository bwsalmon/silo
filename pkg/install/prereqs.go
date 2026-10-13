package install

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/gcefence"
)

// Prereqs is step 1, prereqs (install.md "Prerequisite checks", SV-94):
// it creates nothing, and runs every check, reporting every failure at
// once with what is wrong, why install needs it, and the fix. A check
// whose API isn't enabled yet is deferred, and runs again once the apis
// step has enabled it.
type Prereqs struct{ Env *Env }

// Quota is a limit and its usage.
type Quota struct {
	Limit float64 `json:"limit"`
	Usage float64 `json:"usage"`
}

// Free is how much of q is left.
func (q Quota) Free() float64 { return q.Limit - q.Usage }

// RegionObserved is one of --regions as GCE has it.
type RegionObserved struct {
	Exists bool             `json:"exists"`
	Quotas map[string]Quota `json:"quotas,omitempty"`
	// Zones are the region's zones.
	Zones []string `json:"zones,omitempty"`
	// MachineZone is the first zone offering --control-machine-type,
	// with the type's size; empty if none does.
	MachineZone string `json:"machineZone,omitempty"`
	CPUs        int64  `json:"cpus,omitempty"`
	MemoryMB    int64  `json:"memoryMb,omitempty"`
	// Offered are the machine types of the region's first zone, when
	// none offers --control-machine-type.
	Offered []string `json:"offered,omitempty"`
}

// SubnetObserved is a subnet of the project, outside the VPC silo.
type SubnetObserved struct {
	Name    string   `json:"name"`
	Network string   `json:"network"`
	Region  string   `json:"region"`
	Ranges  []string `json:"ranges"`
}

// PrereqsObserved is everything the checks read.
type PrereqsObserved struct {
	Project    *Project `json:"project,omitempty"`
	ProjectErr string   `json:"projectErr,omitempty"`

	Granted        []string `json:"granted"`
	PermissionsErr string   `json:"permissionsErr,omitempty"`

	BillingEnabled  bool   `json:"billingEnabled,omitempty"`
	BillingDeferred bool   `json:"billingDeferred,omitempty"`
	BillingErr      string `json:"billingErr,omitempty"`

	// ComputeDeferred is set, and the compute reads below are empty,
	// while compute.googleapis.com is off.
	ComputeDeferred bool                       `json:"computeDeferred,omitempty"`
	ComputeErr      string                     `json:"computeErr,omitempty"`
	Regions         map[string]*RegionObserved `json:"regions,omitempty"`
	ProjectQuotas   map[string]Quota           `json:"projectQuotas,omitempty"`
	// Record is the existing install's, from the VPC silo.
	Record           *Record          `json:"record,omitempty"`
	ForeignNetwork   bool             `json:"foreignNetwork,omitempty"`
	Subnets          []SubnetObserved `json:"subnets,omitempty"`
	FirewallPolicies int              `json:"firewallPolicies"`
	// Instances is the project's instances in every region (INSTANCES
	// usage).
	Instances    float64  `json:"instances"`
	MetadataKeys []string `json:"metadataKeys,omitempty"`

	// RuleAttributes is the per-policy rule-attribute quota, from Cloud
	// Quotas; 0 if it isn't reported there.
	RuleAttributes    int64                 `json:"ruleAttributes,omitempty"`
	RuleQuotaDeferred bool                  `json:"ruleQuotaDeferred,omitempty"`
	RuleQuotaErr      string                `json:"ruleQuotaErr,omitempty"`
	SQLDeferred       bool                  `json:"sqlDeferred,omitempty"`
	SQLErr            string                `json:"sqlErr,omitempty"`
	SQLTiers          []SQLTier             `json:"sqlTiers,omitempty"`
	SQLVersions       []string              `json:"sqlVersions,omitempty"`
	SQLInstances      []SQLInstance         `json:"sqlInstances,omitempty"`
	OrgPolicies       map[string]*OrgPolicy `json:"orgPolicies,omitempty"`
	OrgErr            string                `json:"orgErr,omitempty"`
}

// The limits install uses where Google's APIs don't report them, which
// the plan says it assumed.
const (
	// DefaultRuleAttributes is the documented rule-attribute quota of a
	// global network firewall policy.
	DefaultRuleAttributes = 2000
	// DefaultSQLInstances is the documented Cloud SQL instances per
	// project; Cloud SQL Admin doesn't report it.
	DefaultSQLInstances = 1000
	// TreeRuleAttributes is what a 100-silo tree needs (install.md
	// "Prerequisite checks"): about five range attributes per silo.
	TreeRuleAttributes = 500
	// attributesPerSilo is a silo's range attributes with one block
	// (gcp-backend.md "Rule budget").
	attributesPerSilo = 5
	// bootDiskGB is the SSD each control-plane VM's boot disk takes.
	bootDiskGB = 10
)

// The org-policy constraints prereqs reads, with the step each blocks.
const (
	constraintPSCConsumers  = "compute.disablePrivateServiceConnectCreationForConsumers"
	constraintPSCProducer   = "compute.restrictPrivateServiceConnectProducer"
	constraintSACreation    = "iam.disableServiceAccountCreation"
	constraintLocations     = "gcp.resourceLocations"
	constraintTrustedImages = "compute.trustedImageProjects"
	constraintNAT           = "compute.restrictCloudNATUsage"
	constraintMemberDomains = "iam.allowedPolicyMemberDomains"
)

var orgConstraints = []string{constraintPSCConsumers, constraintPSCProducer, constraintSACreation,
	constraintLocations, constraintTrustedImages, constraintNAT, constraintMemberDomains}

// googleProducers is the organization Google's own PSC producers, Cloud
// SQL among them, publish their service attachments from.
const googleProducers = "under:organizations/433637338589"

// rolePermissions are the permissions behind each role install's
// credentials need (install.md "Input"): one or more that install uses
// from each, which testIamPermissions checks.
var rolePermissions = []struct {
	role  string
	perms []string
}{
	{"roles/serviceusage.serviceUsageAdmin", []string{"serviceusage.services.enable", "serviceusage.services.list"}},
	{"roles/compute.admin", []string{"compute.networks.create", "compute.subnetworks.create", "compute.firewallPolicies.create",
		"compute.routers.create", "compute.addresses.create", "compute.forwardingRules.create", "compute.instances.create",
		"compute.instanceGroupManagers.create", "compute.projects.get", "compute.regions.list"}},
	{"roles/dns.admin", []string{"dns.managedZones.create", "dns.changes.create"}},
	{"roles/iam.serviceAccountAdmin", []string{"iam.serviceAccounts.create", "iam.serviceAccounts.get"}},
	{"roles/iam.roleAdmin", []string{"iam.roles.create", "iam.roles.update"}},
	{"roles/resourcemanager.projectIamAdmin", []string{"resourcemanager.projects.getIamPolicy", "resourcemanager.projects.setIamPolicy"}},
	{"roles/artifactregistry.admin", []string{"artifactregistry.repositories.create", "artifactregistry.repositories.setIamPolicy"}},
	{"roles/iap.admin", []string{"iap.tunnelInstances.setIamPolicy"}},
	{"roles/cloudsql.admin", []string{"cloudsql.instances.create", "cloudsql.databases.create", "cloudsql.users.create"}},
	{"roles/browser", []string{"resourcemanager.projects.get"}},
	// Not billing.resourceAssociations.list: a project's
	// testIamPermissions refuses the whole request (400, "not valid for
	// this resource") when it names a permission projects don't carry,
	// which the first real install hit. Reading the billing info is
	// checked by reading it, in the billing check.
}

// Permissions are every permission prereqs tests.
func Permissions() []string {
	var out []string
	for _, r := range rolePermissions {
		out = append(out, r.perms...)
	}
	return out
}

func (s *Prereqs) Name() string          { return "prereqs" }
func (s *Prereqs) NewObserved() Observed { return &PrereqsObserved{} }

// Apply has nothing to do: prereqs only checks.
func (s *Prereqs) Apply(context.Context, []Change) error { return nil }

// PolicyQuota is the firewall policy quota obs implies, which install
// records for silod's rule budget (gcefence.SetQuota): the per-policy
// rule-attribute quota, or the documented default where Cloud Quotas
// doesn't report it.
func PolicyQuota(obs *PrereqsObserved) gcefence.Quota {
	if obs.RuleAttributes > 0 {
		return gcefence.Quota{Attributes: int(obs.RuleAttributes)}
	}
	return gcefence.Quota{Attributes: DefaultRuleAttributes}
}

func (s *Prereqs) Observe(ctx context.Context) (Observed, error) {
	cfg := &s.Env.Config
	cloud := s.Env.Cloud
	project := cfg.Project
	obs := &PrereqsObserved{Granted: []string{}}

	if p, err := cloud.ResourceManager.GetProject(ctx, project); err != nil {
		obs.ProjectErr = err.Error()
	} else {
		obs.Project = p
	}
	if g, err := cloud.ResourceManager.TestIamPermissions(ctx, project, Permissions()); err != nil {
		obs.PermissionsErr = err.Error()
	} else {
		obs.Granted = append(obs.Granted, g...)
		sort.Strings(obs.Granted)
	}
	if on, err := cloud.Billing.BillingEnabled(ctx, project); IsServiceDisabled(err) {
		obs.BillingDeferred = true
	} else if err != nil {
		obs.BillingErr = err.Error()
	} else {
		obs.BillingEnabled = on
	}

	if on, err := s.Env.Enabled(ctx, "compute.googleapis.com"); err != nil {
		return nil, err
	} else if !on {
		obs.ComputeDeferred = true
	} else if err := s.observeCompute(ctx, obs); err != nil {
		obs.ComputeErr = err.Error()
	}
	if on, err := s.Env.Enabled(ctx, "cloudquotas.googleapis.com"); err != nil {
		return nil, err
	} else if !on {
		obs.RuleQuotaDeferred = true
	} else if infos, err := cloud.Compute.ListComputeQuotaInfos(ctx, project); err != nil {
		obs.RuleQuotaErr = err.Error()
	} else {
		obs.RuleAttributes = ruleAttributeQuota(infos)
	}
	if on, err := s.Env.Enabled(ctx, "sqladmin.googleapis.com"); err != nil {
		return nil, err
	} else if !on {
		obs.SQLDeferred = true
	} else if err := s.observeSQL(ctx, obs); err != nil {
		obs.SQLErr = err.Error()
	}
	if obs.Project != nil && obs.Project.Parent != "" {
		obs.OrgPolicies = map[string]*OrgPolicy{}
		for _, c := range orgConstraints {
			p, err := cloud.ResourceManager.EffectiveOrgPolicy(ctx, project, c)
			if err != nil {
				obs.OrgErr = fmt.Sprintf("%s: %v", c, err)
				break
			}
			obs.OrgPolicies[c] = p
		}
	}
	return obs, nil
}

// ruleAttributeQuota finds the network firewall policy rule-attribute
// quota among Cloud Quotas' compute limits.
func ruleAttributeQuota(infos []*gce.QuotaInfo) int64 {
	for _, q := range infos {
		id := strings.ToLower(q.QuotaID + " " + q.Metric)
		if !strings.Contains(id, "firewall") || !strings.Contains(id, "attribute") {
			continue
		}
		for _, d := range q.DimensionsInfos {
			if d.Details.Value > 0 {
				return d.Details.Value
			}
		}
	}
	return 0
}

func (s *Prereqs) observeCompute(ctx context.Context, obs *PrereqsObserved) error {
	cfg := &s.Env.Config
	c := s.Env.Cloud.Compute
	project := cfg.Project

	regions, err := c.ListRegions(ctx, project)
	if err != nil {
		return err
	}
	byName := map[string]bool{}
	for _, r := range cfg.Regions {
		byName[r] = true
	}
	obs.Regions = map[string]*RegionObserved{}
	for _, r := range cfg.Regions {
		obs.Regions[r] = &RegionObserved{}
	}
	for _, r := range regions {
		for _, q := range r.Quotas {
			if q.Metric == gce.MetricInstances {
				obs.Instances += q.Usage
			}
		}
		ro := obs.Regions[r.Name]
		if ro == nil {
			continue
		}
		ro.Exists = true
		ro.Quotas = map[string]Quota{}
		for _, q := range r.Quotas {
			ro.Quotas[q.Metric] = Quota{Limit: q.Limit, Usage: q.Usage}
		}
		for _, z := range r.Zones {
			ro.Zones = append(ro.Zones, gce.LastSegment(z))
		}
		sort.Strings(ro.Zones)
		for _, z := range ro.Zones {
			mt, err := c.GetMachineType(ctx, project, z, cfg.ControlMachineType)
			if gce.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			ro.MachineZone, ro.CPUs, ro.MemoryMB = z, mt.GuestCpus, mt.MemoryMb
			break
		}
		if ro.MachineZone == "" && len(ro.Zones) > 0 {
			mts, err := c.ListMachineTypes(ctx, project, ro.Zones[0])
			if err != nil {
				return err
			}
			for _, mt := range mts {
				ro.Offered = append(ro.Offered, mt.Name)
			}
			sort.Strings(ro.Offered)
		}
	}

	p, err := c.GetProject(ctx, project)
	if err != nil {
		return err
	}
	obs.ProjectQuotas = map[string]Quota{}
	for _, q := range p.Quotas {
		obs.ProjectQuotas[q.Metric] = Quota{Limit: q.Limit, Usage: q.Usage}
	}
	if p.CommonInstanceMetadata != nil {
		for _, it := range p.CommonInstanceMetadata.Items {
			obs.MetadataKeys = append(obs.MetadataKeys, it.Key)
		}
		sort.Strings(obs.MetadataKeys)
	}

	nets, err := c.ListNetworks(ctx, project, gce.ListFilter{})
	if err != nil {
		return err
	}
	subnetRegions := map[string]bool{}
	for _, n := range nets {
		if n.Name == NetworkName {
			if r, ok := ParseRecord(n.Description); ok {
				obs.Record = &r
			} else {
				obs.ForeignNetwork = true
			}
			continue
		}
		for _, sn := range n.Subnetworks {
			subnetRegions[gce.ParseRef(sn).Region] = true
		}
	}
	var sorted []string
	for r := range subnetRegions {
		if r != "" {
			sorted = append(sorted, r)
		}
	}
	sort.Strings(sorted)
	for _, r := range sorted {
		subs, err := c.ListSubnetworks(ctx, project, r, gce.ListFilter{})
		if err != nil {
			return err
		}
		for _, sn := range subs {
			if gce.LastSegment(sn.Network) == NetworkName {
				continue
			}
			so := SubnetObserved{Name: sn.Name, Network: gce.LastSegment(sn.Network), Region: r, Ranges: []string{sn.IpCidrRange}}
			for _, sr := range sn.SecondaryIpRanges {
				so.Ranges = append(so.Ranges, sr.IpCidrRange)
			}
			obs.Subnets = append(obs.Subnets, so)
		}
	}
	pols, err := c.ListFirewallPolicies(ctx, project)
	if err != nil {
		return err
	}
	obs.FirewallPolicies = len(pols)
	return nil
}

func (s *Prereqs) observeSQL(ctx context.Context, obs *PrereqsObserved) error {
	sql := s.Env.Cloud.SQLAdmin
	project := s.Env.Config.Project
	var err error
	if obs.SQLTiers, err = sql.ListTiers(ctx, project); err != nil {
		return err
	}
	if obs.SQLVersions, err = sql.ListDatabaseVersions(ctx, project); err != nil {
		return err
	}
	sort.Strings(obs.SQLVersions)
	insts, err := sql.ListInstances(ctx, project)
	if err != nil {
		return err
	}
	obs.SQLInstances = insts
	return nil
}

// checker collects a plan's checks.
type checker struct {
	project string
	ok      []string
	out     []Change
}

func (k *checker) pass(format string, args ...any) { k.ok = append(k.ok, fmt.Sprintf(format, args...)) }

func (k *checker) fail(check, detail, why, fix string) {
	k.out = append(k.out, Change{Action: ActionFail, Object: check + ":", Detail: detail, Why: why, Fix: fix})
}

func (k *checker) warn(check, detail, why, fix string) {
	k.out = append(k.out, Change{Action: ActionWarn, Object: check + ":", Detail: detail, Why: why, Fix: fix})
}

func (k *checker) deferred(checks, service string) {
	k.out = append(k.out, Change{Action: ActionDefer, Object: checks + ":",
		Detail: fmt.Sprintf("checked once the apis step has enabled %s", service)})
}

func (s *Prereqs) quotaPage() string {
	return "https://console.cloud.google.com/iam-admin/quotas?project=" + s.Env.Config.Project
}

func (s *Prereqs) Plan(o Observed) []Change {
	obs := o.(*PrereqsObserved)
	k := &checker{project: s.Env.Config.Project}
	s.checkAdmins(k, obs)
	s.checkCredentials(k, obs)
	s.checkBilling(k, obs)
	switch {
	case obs.ComputeDeferred:
		k.deferred("regions, quota, VPC, firewall policy, address space and project checks", "compute.googleapis.com")
	case obs.ComputeErr != "":
		k.fail("compute", "couldn't read the project's compute resources: "+obs.ComputeErr,
			"the region, quota, VPC and address-space checks read them", "check the credentials' compute permissions, then run install again")
	default:
		s.checkRegions(k, obs)
		s.checkQuota(k, obs)
		s.checkAgentCapacity(k, obs)
		s.checkVPCLimits(k, obs)
		s.checkFirewallPolicy(k, obs)
		s.checkAddressSpace(k, obs)
		s.checkExisting(k, obs)
		s.checkDedicated(k, obs)
		s.checkMetadata(k, obs)
	}
	s.checkSQL(k, obs)
	s.checkOrgPolicies(k, obs)
	return append([]Change{{Action: ActionOK, Detail: strings.Join(k.ok, "; ")}}, k.out...)
}

func (s *Prereqs) firstInstall(obs *PrereqsObserved) bool { return obs.Record == nil }

func (s *Prereqs) checkAdmins(k *checker, obs *PrereqsObserved) {
	if len(s.Env.Config.Admins) == 0 && s.firstInstall(obs) {
		k.fail("admin", "no --admin, and this is the first install",
			"the root needs a first member, or nobody can manage the tree",
			"add --admin user:<you@example.com> (repeatable)")
	}
}

func (s *Prereqs) checkCredentials(k *checker, obs *PrereqsObserved) {
	if obs.PermissionsErr != "" {
		k.fail("credentials", "couldn't test the credentials' permissions: "+obs.PermissionsErr,
			"install checks its permissions before it changes anything",
			"check --credentials (or Application Default Credentials: gcloud auth application-default login)")
		return
	}
	granted := map[string]bool{}
	for _, p := range obs.Granted {
		granted[p] = true
	}
	var missing []string
	roles := map[string]bool{}
	var roleList []string
	for _, r := range rolePermissions {
		for _, p := range r.perms {
			if !granted[p] {
				missing = append(missing, p)
				if !roles[r.role] {
					roles[r.role] = true
					roleList = append(roleList, r.role)
				}
			}
		}
	}
	if len(missing) == 0 {
		k.pass("credentials ok")
		return
	}
	k.fail("credentials", "missing "+strings.Join(missing, ", "),
		"install creates and configures every object it sets up with these credentials",
		fmt.Sprintf("on project %s, grant roles/owner, or each of: %s (gcloud projects add-iam-policy-binding %s --member <you> --role <role>)",
			k.project, strings.Join(roleList, "; "), k.project))
}

func (s *Prereqs) checkBilling(k *checker, obs *PrereqsObserved) {
	switch {
	case obs.BillingDeferred:
		k.deferred("billing", "cloudbilling.googleapis.com")
	case obs.BillingErr != "":
		k.fail("billing", "couldn't read the project's billing: "+obs.BillingErr,
			"GCE, Cloud SQL and Artifact Registry need a billing account",
			"grant billing.resourceAssociations.list, then run install again")
	case !obs.BillingEnabled:
		k.fail("billing", "billing is not enabled on "+k.project,
			"GCE, Cloud SQL and Artifact Registry need a billing account",
			"link one at https://console.cloud.google.com/billing/linkedaccount?project="+k.project)
	default:
		k.pass("billing on")
	}
}

func (s *Prereqs) checkRegions(k *checker, obs *PrereqsObserved) {
	mt := s.Env.Config.ControlMachineType
	for _, name := range s.Env.Config.Regions {
		r := obs.Regions[name]
		switch {
		case r == nil || !r.Exists:
			k.fail("regions", "region "+name+" doesn't exist",
				"the root's regions are where its silos may run", "check --regions (gcloud compute regions list)")
		case r.MachineZone == "":
			k.fail("regions", fmt.Sprintf("%s offers %s in none of its zones", name, mt),
				"the control VMs run on --control-machine-type, and any region may hold them",
				fmt.Sprintf("pick a --control-machine-type it offers: %s", strings.Join(r.Offered, ", ")))
		}
	}
}

// controlPlane is what the control plane takes from the home region's
// quotas: the control VMs, the gateway replicas and the short-lived
// migration VM, each of the control machine type.
func (s *Prereqs) controlPlane(cpus int64) map[string]float64 {
	cfg := &s.Env.Config
	vms := float64(cfg.ControlReplicas + cfg.GatewayReplicas + 1)
	addrs := 2.0 // silo-gateway-ip, silo-db-psc
	if cfg.ControlReplicas > 1 {
		addrs++
	}
	if cfg.GatewayReplicas > 0 {
		addrs++
	}
	return map[string]float64{
		gce.MetricCPUS:       vms * float64(cpus),
		gce.MetricInstances:  vms,
		"STATIC_ADDRESSES":   addrs,
		gce.MetricSSDTotalGB: vms * bootDiskGB,
	}
}

func (s *Prereqs) checkQuota(k *checker, obs *PrereqsObserved) {
	home := s.Env.Config.HomeRegion()
	hr := obs.Regions[home]
	if hr != nil && hr.Exists {
		if q, ok := hr.Quotas[gce.MetricCPUS]; ok {
			k.pass("CPUS %s %s/%s used", home, num(q.Usage), num(q.Limit))
		}
		// On a re-run the control plane already counts in the usage.
		if s.firstInstall(obs) && hr.MachineZone != "" {
			need := s.controlPlane(hr.CPUs)
			for _, m := range []string{gce.MetricCPUS, gce.MetricInstances, "STATIC_ADDRESSES", gce.MetricSSDTotalGB} {
				q, ok := hr.Quotas[m]
				if !ok || q.Free() >= need[m] {
					continue
				}
				k.fail("quota", fmt.Sprintf("%s in %s: %s of %s used, and the control plane needs %s more",
					m, home, num(q.Usage), num(q.Limit), num(need[m])),
					"the control VMs, gateway replicas and the migration VM run in the home region",
					"raise it at "+s.quotaPage())
			}
		}
	}
	for _, name := range s.Env.Config.Regions {
		r := obs.Regions[name]
		if r == nil || !r.Exists {
			continue
		}
		if q, ok := r.Quotas[gce.MetricCPUS]; ok && q.Limit == 0 {
			k.fail("quota", "CPUS in "+name+" is 0",
				"a region with no CPUs gives the root's silos nothing to run there",
				"raise it at "+s.quotaPage()+", or leave "+name+" out of --regions")
		}
	}
}

func (s *Prereqs) checkAgentCapacity(k *checker, obs *PrereqsObserved) {
	cfg := &s.Env.Config
	if cfg.AgentSessions == 0 {
		return
	}
	hr := obs.Regions[cfg.HomeRegion()]
	if hr == nil || hr.MachineZone == "" {
		return
	}
	per := SessionsPerVM(hr.CPUs, hr.MemoryMB)
	total := per * cfg.ControlReplicas
	need := cfg.AgentSessions
	replicas := 0
	if per > 0 {
		replicas = (need + per - 1) / per
	}
	fix := fmt.Sprintf("each %s carries %d sessions: use --control-replicas %d, or a larger --control-machine-type", cfg.ControlMachineType, per, replicas+1)
	switch {
	case need > total:
		k.fail("agent capacity", fmt.Sprintf("%d sessions, but %d %s VMs carry %d", need, cfg.ControlReplicas, cfg.ControlMachineType, total),
			"each session takes 0.2 vCPU and 0.5 GB, and each VM keeps 1 vCPU and 2 GB for silod and the gateway", fix)
	case need > per*(cfg.ControlReplicas-1):
		k.warn("agent capacity", fmt.Sprintf("%d sessions fit %d %s VMs, but not with one VM down", need, cfg.ControlReplicas, cfg.ControlMachineType),
			"while a control VM restarts or fails, its sessions move to the others", fix)
	}
}

// SessionsPerVM is how many agent sessions a control VM of cpus and
// memoryMB carries (install.md "Prerequisite checks", agent capacity):
// 0.2 vCPU and 0.5 GB per session, after 1 vCPU and 2 GB for silod and
// the gateway. It is agents.SessionsPerVM, which silod also counts its
// own slots with.
func SessionsPerVM(cpus, memoryMB int64) int { return agents.SessionsPerVM(cpus, memoryMB) }

func (s *Prereqs) checkVPCLimits(k *checker, obs *PrereqsObserved) {
	fix := "raise it at " + s.quotaPage()
	pq := obs.ProjectQuotas
	if q, ok := pq[gce.MetricNetworks]; ok {
		k.pass("NETWORKS %s/%s", num(q.Usage), num(q.Limit))
		if obs.Record == nil && !obs.ForeignNetwork && q.Free() < 1 {
			k.fail("VPC limits", fmt.Sprintf("NETWORKS: %s of %s used, and install needs 1 for %s", num(q.Usage), num(q.Limit), NetworkName),
				"every silo shares the one VPC install makes", fix)
		}
	}
	for _, m := range []string{gce.MetricSubnetworks, gce.MetricFirewalls} {
		if q, ok := pq[m]; ok && q.Free() < q.Limit/10 {
			k.fail("VPC limits", fmt.Sprintf("%s: %s of %s used, less than 10%% left", m, num(q.Usage), num(q.Limit)),
				"every silo's subnets and the tree's firewall rules come out of the project's limits", fix)
		}
	}
	if q, ok := pq["ROUTERS"]; ok && obs.Record == nil && q.Free() < 1 {
		k.fail("VPC limits", fmt.Sprintf("ROUTERS: %s of %s used, and install needs 1 for NAT", num(q.Usage), num(q.Limit)),
			"the root's egress goes through Cloud NAT on a router in the home region", fix)
	}
}

func (s *Prereqs) checkFirewallPolicy(k *checker, obs *PrereqsObserved) {
	for m, q := range obs.ProjectQuotas {
		if !strings.Contains(m, "FIREWALL_POLIC") || obs.Record != nil {
			continue
		}
		if q.Free() < 1 {
			k.fail("firewall policy limits", fmt.Sprintf("%s: %s of %s used, and install needs 1 for the fence", m, num(q.Usage), num(q.Limit)),
				"the fence between silos is one global network firewall policy", "raise it at "+s.quotaPage())
		}
	}
	switch {
	case obs.RuleQuotaDeferred:
		k.deferred("firewall policy rule quota", "cloudquotas.googleapis.com")
		return
	case obs.RuleQuotaErr != "":
		k.fail("firewall policy limits", "couldn't read Cloud Quotas: "+obs.RuleQuotaErr,
			"install records the policy's rule quota for silod's rule budget", "check the credentials, then run install again")
		return
	}
	q := PolicyQuota(obs)
	silos := q.Attributes * gcefence.BudgetPercent / 100 / attributesPerSilo
	note := ""
	if obs.RuleAttributes == 0 {
		note = " (not reported by Cloud Quotas: the documented default)"
	}
	k.pass("firewall policy rule attributes %d%s, for about %d silos", q.Attributes, note, silos)
	if q.Attributes < TreeRuleAttributes {
		k.fail("firewall policy limits", fmt.Sprintf("the per-policy rule-attribute quota is %d, which allows a tree of about %d silos; a 100-silo tree needs %d",
			q.Attributes, silos, TreeRuleAttributes),
			"the fence's rules grow with the tree, and silod refuses changes past 90% of the quota", "raise it at "+s.quotaPage())
	}
}

func (s *Prereqs) checkAddressSpace(k *checker, obs *PrereqsObserved) {
	space := s.Env.Config.AddressSpace
	for _, sn := range obs.Subnets {
		for _, r := range sn.Ranges {
			p := parsePrefix(r)
			if p.IsValid() && p.Overlaps(space) {
				k.fail("address space", fmt.Sprintf("--address-space %s overlaps subnet %s (%s) of VPC %s in %s", space, sn.Name, r, sn.Network, sn.Region),
					"the root's block must be the project's alone, or silos' addresses would collide with it",
					"pick another --address-space, or delete the subnet")
			}
		}
	}
}

func (s *Prereqs) checkExisting(k *checker, obs *PrereqsObserved) {
	if obs.ForeignNetwork {
		k.fail("existing install", "the project has a VPC named "+NetworkName+" that install didn't create",
			"install owns the VPC "+NetworkName, "delete it, or install into another project")
	}
	if obs.Record == nil {
		return
	}
	now := RecordOf(s.Env)
	for _, d := range obs.Record.Differences(now) {
		k.fail("existing install", d,
			"it can't be changed in place: the root's block, the subnets and the control VMs depend on it",
			"run again with the install's value, or silo uninstall first")
	}
}

func (s *Prereqs) checkDedicated(k *checker, obs *PrereqsObserved) {
	if s.firstInstall(obs) && obs.Instances > 0 {
		k.warn("dedicated project", fmt.Sprintf("the project already has %s instances outside Silo", num(obs.Instances)),
			"they share the project's quota with every silo", "install into a project dedicated to Silo")
	}
}

// gceMetadataKeys are project metadata keys GCE itself sets or reads.
var gceMetadataKeys = map[string]bool{"enable-oslogin": true, "enable-oslogin-2fa": true,
	"enable-guest-attributes": true, "enable-os-inventory": true, "serial-port-enable": true, "block-project-ssh-keys": true}

func (s *Prereqs) checkMetadata(k *checker, obs *PrereqsObserved) {
	var other []string
	for _, key := range obs.MetadataKeys {
		if !strings.HasPrefix(key, "google-") && !gceMetadataKeys[key] {
			other = append(other, key)
		}
	}
	if len(other) > 0 {
		k.warn("project metadata", "commonInstanceMetadata has "+strings.Join(other, ", "),
			"every silo's VMs can read project metadata", "move it to instance metadata, or remove it")
	}
}

func (s *Prereqs) checkSQL(k *checker, obs *PrereqsObserved) {
	cfg := &s.Env.Config
	switch {
	case obs.SQLDeferred:
		k.deferred("Cloud SQL", "sqladmin.googleapis.com")
		return
	case obs.SQLErr != "":
		k.fail("Cloud SQL", "couldn't read Cloud SQL: "+obs.SQLErr,
			"silod's state is a Cloud SQL instance", "check that sqladmin.googleapis.com can be enabled, then run install again")
		return
	}
	home := cfg.HomeRegion()
	var tiers []string
	found := false
	for _, t := range obs.SQLTiers {
		for _, r := range t.Regions {
			if r == home {
				tiers = append(tiers, t.Tier)
				found = found || t.Tier == cfg.DBTier
			}
		}
	}
	if !found {
		k.fail("Cloud SQL", fmt.Sprintf("%s doesn't offer --db-tier %s", home, cfg.DBTier),
			"the database lives in the home region", "pick one it offers: "+strings.Join(tiers, ", "))
	}
	if !contains(obs.SQLVersions, cfg.DBVersion) {
		var pg []string
		for _, v := range obs.SQLVersions {
			if strings.HasPrefix(v, "POSTGRES_") {
				pg = append(pg, v)
			}
		}
		k.fail("Cloud SQL", "Cloud SQL doesn't offer --db-version "+cfg.DBVersion,
			"silod's schema is PostgreSQL", "pick one it offers: "+strings.Join(pg, ", "))
	}
	if n := len(obs.SQLInstances); n >= DefaultSQLInstances {
		k.fail("Cloud SQL", fmt.Sprintf("the project has %d Cloud SQL instances, the documented default quota", n),
			"install creates one", "raise it at "+s.quotaPage())
	}
	// An instance of this install's that has been migrated (its
	// silo-schema label isn't 0), while the VPC carries no install
	// record, was kept by an uninstall. One this install created and
	// hasn't migrated yet is an interrupted first install's, and the
	// install goes on with it.
	var kept []string
	for _, in := range obs.SQLInstances {
		if in.Labels[LabelKey] == s.Env.ID && in.Labels[RoleLabel] == RoleDB && in.Labels[SchemaLabel] != "0" {
			kept = append(kept, in.Name)
		}
	}
	switch {
	case cfg.DBInstance != "":
		ok := false
		for _, in := range obs.SQLInstances {
			ok = ok || in.Name == cfg.DBInstance
		}
		if !ok {
			k.fail("Cloud SQL", "--db-instance "+cfg.DBInstance+" doesn't exist in "+k.project,
				"install adopts an instance only from its own project", "check the name: gcloud sql instances list --project "+k.project)
		}
	case len(kept) > 0 && s.firstInstall(obs):
		k.fail("Cloud SQL", "an earlier install's database was kept: "+strings.Join(kept, ", "),
			"it holds the tree's grants, policies and audit, and install won't make a second",
			fmt.Sprintf("adopt it with --db-instance %s, or delete it with silo uninstall --project %s --delete-data", kept[0], k.project))
	}
}

func (s *Prereqs) checkOrgPolicies(k *checker, obs *PrereqsObserved) {
	if obs.ProjectErr != "" {
		k.fail("org policy", "couldn't read the project: "+obs.ProjectErr,
			"install checks the organization's policies before it relies on them", "grant roles/browser on the project")
		return
	}
	if obs.Project == nil || obs.Project.Parent == "" {
		k.pass("no organization")
		return
	}
	if obs.OrgErr != "" {
		k.fail("org policy", "couldn't read the effective org policies: "+obs.OrgErr,
			"the project's organization may forbid a step", "grant orgpolicy.policy.get on the project, or ask the organization's admin")
		return
	}
	pol := obs.OrgPolicies
	before := len(k.out)
	block := func(constraint, step, detail string) {
		fix := "ask the organization's admin to exempt the project from constraints/" + constraint
		if strings.HasPrefix(constraint, "compute.") && strings.Contains(constraint, "PrivateServiceConnect") {
			k.fail("PSC", "constraints/"+constraint+" "+detail,
				"silod reaches its database through a Private Service Connect endpoint (Private services access, the alternative, isn't built in v1)", fix)
			return
		}
		k.fail("org policy", "constraints/"+constraint+" "+detail, "it blocks the "+step+" step", fix)
	}
	if p := pol[constraintPSCConsumers]; p != nil && (p.AllowAll || contains(p.Allowed, "SERVICE_PRODUCERS")) {
		block(constraintPSCConsumers, "db-endpoint", "disables endpoints to service producers")
	}
	if p := pol[constraintPSCProducer]; p.Restricts() && !p.Permits(googleProducers) {
		block(constraintPSCProducer, "db-endpoint", "doesn't allow Google's producers ("+googleProducers+")")
	}
	if p := pol[constraintSACreation]; p != nil && p.Enforced {
		block(constraintSACreation, "service-account", "forbids creating silod's service account")
	}
	if p := pol[constraintLocations]; p.Restricts() {
		for _, r := range s.Env.Config.Regions {
			if !p.Permits(locationValues(r)...) {
				block(constraintLocations, "network", "excludes "+r+", one of --regions")
			}
		}
	}
	if p := pol[constraintTrustedImages]; p.Restricts() && !p.Permits("projects/cos-cloud") {
		block(constraintTrustedImages, "control", "doesn't trust projects/cos-cloud, the control VMs' image")
	}
	if p := pol[constraintNAT]; p.Restricts() {
		k.warn("org policy", "constraints/"+constraintNAT+" restricts Cloud NAT",
			"the root's egress goes through Cloud NAT on the gateway subnet", "ask the organization's admin to allow it for the project")
	}
	if p := pol[constraintMemberDomains]; p.Restricts() {
		k.warn("org policy", "constraints/"+constraintMemberDomains+" restricts IAM members",
			"--admin and --front-door-member are granted IAM roles", "use identities in the organization's allowed domains")
	}
	if len(k.out) == before {
		k.pass("org policies ok")
	}
}

// locationValues are the gcp.resourceLocations values that match
// region: itself, its value group, and its multi-region group.
func locationValues(region string) []string {
	out := []string{region, "in:" + region + "-locations"}
	if i := strings.Index(region, "-"); i > 0 {
		out = append(out, "in:"+region[:i]+"-locations")
	}
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// num prints a quota number without a fraction where it has none.
func num(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
