package install

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/silo/egress"
)

// Config is silo install's input (install.md "Input", SV-90): one field
// per flag, with the flag's default from Defaults.
type Config struct {
	Project string
	// Regions are the root's allowed regions. The first is the home
	// region.
	Regions []string
	// Admins are the first root members, user:... or serviceAccount:....
	// Required on the first install only, which prereqs checks.
	Admins []string
	// Credentials is a service-account key file; empty means
	// Application Default Credentials.
	Credentials string

	AddressSpace netip.Prefix
	GatewayRange netip.Prefix
	ControlRange netip.Prefix

	// RootMemoryMB is the root's memory_mb and spot_memory_mb per
	// region; 0 is unlimited.
	RootMemoryMB    int64
	EgressAllow     []string
	RootGrantPolicy string

	Version            string
	Image              string
	HarnessImage       string
	ControlMachineType string
	ControlReplicas    int
	AgentSessions      int
	RootModelBudget    float64
	BudgetPeriod       string
	GatewayReplicas    int

	DBTier              string
	DBVersion           string
	DBHA                bool
	DBBackupRetention   int
	DBMaintenanceWindow string
	DBInstance          string

	FrontDoorMembers []string
	AuditSink        bool

	DryRun bool
	Yes    bool
	// Output is table or json; json is for --dry-run.
	Output string
}

// Defaults is the Config every flag defaults to (install.md "Input").
func Defaults() Config {
	return Config{
		AddressSpace:        netip.MustParsePrefix("10.0.0.0/8"),
		GatewayRange:        netip.MustParsePrefix("10.255.255.0/24"),
		ControlRange:        netip.MustParsePrefix("10.255.254.0/24"),
		RootGrantPolicy:     "exports=allow,links=deny",
		ControlMachineType:  "e2-standard-2",
		ControlReplicas:     1,
		BudgetPeriod:        "month",
		DBTier:              "db-g1-small",
		DBVersion:           "POSTGRES_17",
		DBBackupRetention:   14,
		DBMaintenanceWindow: "sun:05:00",
		Output:              "table",
	}
}

// HomeRegion is the first of Regions: where the control VM, the gateway
// subnet and NAT live.
func (c *Config) HomeRegion() string {
	if len(c.Regions) == 0 {
		return ""
	}
	return c.Regions[0]
}

// Flags registers every install flag on fs, writing into c, which
// should start as Defaults(). version is the CLI's own release, the
// default of --version.
func (c *Config) Flags(fs *flag.FlagSet, version string) {
	if c.Version == "" {
		c.Version = version
	}
	fs.StringVar(&c.Project, "project", c.Project, "the host project, dedicated to Silo (required)")
	fs.Var((*listFlag)(&c.Regions), "regions", "the root's regions, comma-separated; the first is the home region (required)")
	fs.Var((*listFlag)(&c.Admins), "admin", "a first root member, user:... or serviceAccount:... (repeatable; required on first install)")
	fs.StringVar(&c.Credentials, "credentials", c.Credentials, "a service-account key file (default Application Default Credentials)")
	fs.Var((*prefixFlag)(&c.AddressSpace), "address-space", "the shared VPC's range, the root's block")
	fs.Var((*prefixFlag)(&c.GatewayRange), "gateway-range", "the gateway subnet's range, inside --address-space")
	fs.Var((*prefixFlag)(&c.ControlRange), "control-range", "the control subnet's range, inside --address-space")
	fs.Var((*memoryFlag)(&c.RootMemoryMB), "root-memory", "the root's memory per region: unlimited, or MB, or with an MB or GB suffix")
	fs.Var((*listFlag)(&c.EgressAllow), "egress-allow", "the root's egress allowlist (repeatable or comma-separated)")
	fs.StringVar(&c.RootGrantPolicy, "root-grant-policy", c.RootGrantPolicy, "the root's grant policy")
	fs.StringVar(&c.Version, "version", c.Version, "the silod release to deploy")
	fs.StringVar(&c.Image, "image", c.Image, "a silod image to deploy instead, by digest (development)")
	fs.StringVar(&c.HarnessImage, "harness-image", c.HarnessImage, "with --agent-sessions, a harness image to deploy instead, by digest (development; needed with --image)")
	fs.StringVar(&c.ControlMachineType, "control-machine-type", c.ControlMachineType, "the control VM's machine type")
	fs.IntVar(&c.ControlReplicas, "control-replicas", c.ControlReplicas, "control VMs (silod and a gateway each)")
	fs.IntVar(&c.AgentSessions, "agent-sessions", c.AgentSessions, "agent harness sessions the control VMs may run at once; 0 turns agents off")
	fs.Float64Var(&c.RootModelBudget, "root-model-budget", c.RootModelBudget, "the root's model budget, US dollars per period")
	fs.StringVar(&c.BudgetPeriod, "budget-period", c.BudgetPeriod, "the model budget period: month, week or day (UTC)")
	fs.IntVar(&c.GatewayReplicas, "gateway-replicas", c.GatewayReplicas, "extra gateway VMs behind an internal load balancer")
	fs.StringVar(&c.DBTier, "db-tier", c.DBTier, "the Cloud SQL machine tier")
	fs.StringVar(&c.DBVersion, "db-version", c.DBVersion, "the Cloud SQL database version")
	fs.BoolVar(&c.DBHA, "db-ha", c.DBHA, "a regional (HA) Cloud SQL instance")
	fs.IntVar(&c.DBBackupRetention, "db-backup-retention", c.DBBackupRetention, "daily backups Cloud SQL keeps")
	fs.StringVar(&c.DBMaintenanceWindow, "db-maintenance-window", c.DBMaintenanceWindow, "when Cloud SQL may restart for maintenance, day:HH:00 UTC")
	fs.StringVar(&c.DBInstance, "db-instance", c.DBInstance, "adopt this existing Cloud SQL instance instead of creating one")
	fs.Var((*listFlag)(&c.FrontDoorMembers), "front-door-member", "an extra identity granted the IAP tunnel to the front door (repeatable)")
	fs.BoolVar(&c.AuditSink, "audit-sink", c.AuditSink, "also stream audit records to Cloud Logging")
	fs.BoolVar(&c.DryRun, "dry-run", c.DryRun, "print the plan and change nothing")
	fs.BoolVar(&c.Yes, "yes", c.Yes, "don't ask for confirmation before applying")
	fs.StringVar(&c.Output, "output", c.Output, "the plan's format: table or json")
	fs.StringVar(&c.Output, "o", c.Output, "short for --output")
}

var (
	projectRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	regionRE   = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	machineRE  = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9-]+$`)
	tierRE     = regexp.MustCompile(`^db-[a-zA-Z0-9-]+$`)
	dbVerRE    = regexp.MustCompile(`^POSTGRES_([0-9]+)$`)
	digestRE   = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	instanceRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,97}$`)
	policyRE   = regexp.MustCompile(`^exports=(allow|deny),links=(allow|deny)$`)
)

// Validate checks every flag on its own and against the others, and
// returns every problem at once. It reads nothing from the cloud: what
// needs the project is prereqs'.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.Project == "" {
		bad("--project is required")
	} else if !projectRE.MatchString(c.Project) {
		bad("--project %q is not a project id", c.Project)
	}
	if len(c.Regions) == 0 {
		bad("--regions is required")
	}
	seen := map[string]bool{}
	for _, r := range c.Regions {
		if !regionRE.MatchString(r) {
			bad("--regions: %q is not a region name", r)
		}
		if seen[r] {
			bad("--regions: %s is given twice", r)
		}
		seen[r] = true
	}
	for _, a := range c.Admins {
		if !strings.HasPrefix(a, "user:") && !strings.HasPrefix(a, "serviceAccount:") || !strings.Contains(a, "@") {
			bad("--admin %q: want user:<email> or serviceAccount:<email>", a)
		}
	}
	for _, m := range c.FrontDoorMembers {
		ok := false
		for _, p := range []string{"user:", "serviceAccount:", "group:", "domain:"} {
			ok = ok || strings.HasPrefix(m, p) && len(m) > len(p)
		}
		if !ok {
			bad("--front-door-member %q: want user:, serviceAccount:, group: or domain:", m)
		}
	}
	rangesOK := true
	for _, r := range []struct {
		flag string
		p    netip.Prefix
	}{{"--address-space", c.AddressSpace}, {"--gateway-range", c.GatewayRange}, {"--control-range", c.ControlRange}} {
		if !r.p.IsValid() || !r.p.Addr().Is4() || r.p.Masked() != r.p {
			bad("%s %s is not an IPv4 network address and prefix length", r.flag, r.p)
			rangesOK = false
		}
	}
	if rangesOK {
		for _, r := range []struct {
			flag string
			p    netip.Prefix
		}{{"--gateway-range", c.GatewayRange}, {"--control-range", c.ControlRange}} {
			if !Contains(c.AddressSpace, r.p) {
				bad("%s %s is not inside --address-space %s", r.flag, r.p, c.AddressSpace)
			}
		}
		if c.GatewayRange.Overlaps(c.ControlRange) {
			bad("--gateway-range %s and --control-range %s overlap", c.GatewayRange, c.ControlRange)
		}
	}
	if c.RootMemoryMB < 0 {
		bad("--root-memory can't be negative")
	}
	for _, e := range c.EgressAllow {
		if _, err := egress.Parse(e); err != nil {
			bad("--egress-allow: %v", err)
		}
	}
	if !policyRE.MatchString(c.RootGrantPolicy) {
		bad("--root-grant-policy %q: want exports=allow|deny,links=allow|deny", c.RootGrantPolicy)
	}
	if c.Image != "" && !digestRE.MatchString(c.Image) {
		bad("--image %q must name an image by digest (<repository>@sha256:<64 hex digits>)", c.Image)
	}
	if c.HarnessImage != "" && !digestRE.MatchString(c.HarnessImage) {
		bad("--harness-image %q must name an image by digest (<repository>@sha256:<64 hex digits>)", c.HarnessImage)
	}
	if c.AgentSessions > 0 && c.Image != "" && c.HarnessImage == "" {
		bad("--agent-sessions with --image needs --harness-image too: the harness image is built beside silod's (make silod-harness-image)")
	}
	if c.Image == "" && c.Version == "" {
		bad("--version is required: this CLI was built without a release version")
	}
	if !machineRE.MatchString(c.ControlMachineType) {
		bad("--control-machine-type %q is not a machine type name", c.ControlMachineType)
	}
	if c.ControlReplicas < 1 {
		bad("--control-replicas must be at least 1")
	}
	if c.AgentSessions < 0 {
		bad("--agent-sessions can't be negative")
	}
	if c.RootModelBudget < 0 {
		bad("--root-model-budget can't be negative")
	}
	switch c.BudgetPeriod {
	case "month", "week", "day":
	default:
		bad("--budget-period %q: want month, week or day", c.BudgetPeriod)
	}
	if c.GatewayReplicas < 0 {
		bad("--gateway-replicas can't be negative")
	}
	if !tierRE.MatchString(c.DBTier) {
		bad("--db-tier %q is not a Cloud SQL tier (db-g1-small, db-custom-1-3840, ...)", c.DBTier)
	}
	if !dbVerRE.MatchString(c.DBVersion) {
		bad("--db-version %q: want POSTGRES_<major>, such as POSTGRES_17", c.DBVersion)
	}
	if c.DBBackupRetention < 1 || c.DBBackupRetention > 365 {
		bad("--db-backup-retention must be between 1 and 365")
	}
	if _, _, err := MaintenanceWindow(c.DBMaintenanceWindow); err != nil {
		bad("--db-maintenance-window: %v", err)
	}
	if c.DBInstance != "" && !instanceRE.MatchString(c.DBInstance) {
		bad("--db-instance %q is not a Cloud SQL instance name", c.DBInstance)
	}
	switch c.Output {
	case "table", "json":
	default:
		bad("--output %q: want table or json", c.Output)
	}
	return errors.Join(errs...)
}

var days = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// MaintenanceWindow parses day:HH:00 into Cloud SQL's day (1 is Monday,
// 7 Sunday) and hour. Cloud SQL's windows start on the hour.
func MaintenanceWindow(s string) (day, hour int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, 0, fmt.Errorf("%q: want day:HH:00, such as sun:05:00", s)
	}
	for i, d := range days {
		if strings.EqualFold(parts[0], d) {
			day = i + 1
		}
	}
	if day == 0 {
		return 0, 0, fmt.Errorf("%q: the day is one of %s", s, strings.Join(days, ", "))
	}
	hour, err = strconv.Atoi(parts[1])
	if err != nil || len(parts[1]) != 2 || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("%q: the hour is 00 to 23", s)
	}
	if parts[2] != "00" {
		return 0, 0, fmt.Errorf("%q: Cloud SQL's windows start on the hour", s)
	}
	return day, hour, nil
}

// Contains reports whether inner lies wholly inside outer.
func Contains(outer, inner netip.Prefix) bool {
	return outer.IsValid() && inner.IsValid() && inner.Bits() >= outer.Bits() && outer.Contains(inner.Addr())
}

// InstallID is the install's id: the value of the silo-install label
// on everything install creates. It is derived from the project, so a
// re-run, or a later install after an uninstall that kept the
// database, carries the same id with nothing stored to find it by.
func InstallID(project string) string {
	sum := sha256.Sum256([]byte("silo-install:" + project))
	return "i" + hex.EncodeToString(sum[:])[:10]
}

// listFlag is a repeatable flag whose values may also be
// comma-separated.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// prefixFlag is a CIDR flag.
type prefixFlag netip.Prefix

func (p *prefixFlag) String() string {
	if !netip.Prefix(*p).IsValid() {
		return ""
	}
	return netip.Prefix(*p).String()
}

func (p *prefixFlag) Set(v string) error {
	pf, err := netip.ParsePrefix(v)
	if err != nil {
		return fmt.Errorf("%q is not a CIDR range", v)
	}
	if pf.Masked() != pf {
		return fmt.Errorf("%s has host bits set: did you mean %s?", pf, pf.Masked())
	}
	*p = prefixFlag(pf)
	return nil
}

// memoryFlag is --root-memory: unlimited (0), or MB.
type memoryFlag int64

func (m *memoryFlag) String() string {
	if *m == 0 {
		return "unlimited"
	}
	return strconv.FormatInt(int64(*m), 10) + "MB"
}

func (m *memoryFlag) Set(v string) error {
	s := strings.ToUpper(strings.TrimSpace(v))
	if s == "UNLIMITED" {
		*m = 0
		return nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1024, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		s = strings.TrimSuffix(s, "MB")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("%q: want unlimited, or a positive size in MB or GB", v)
	}
	*m = memoryFlag(n * mult)
	return nil
}
