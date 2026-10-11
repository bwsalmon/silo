package install

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/gateway"
	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/gwtls"
	compute "google.golang.org/api/compute/v1"
)

// The control VM's metadata keys install owns.
const (
	// ImageKey holds the silod image the VM runs, by digest. Changing
	// it restarts both containers (the VM's silo-image-watch unit).
	ImageKey = "silo-image"
	// CredentialKeyKey holds the key to the agent credentials kept in
	// the database, with --agent-sessions (agents.md, "Credentials").
	CredentialKeyKey = "silo-credential-key"
	userDataKey      = "user-data"
	// UserDataHashKey is a hash of user-data, so that the plan can say
	// whether the VM's boot configuration is this release's without
	// carrying the whole of it.
	UserDataHashKey = "silo-user-data-sha256"
)

// The users and group the control VM's containers run as: silod and the
// gateway apart, and a group of the gateway's that silod also holds, so
// that the gateway, and only it, can open silod's gateway socket.
const (
	SilodUID     = 2001
	GatewayUID   = 2002
	GatewayGroup = 2000
)

// controlDisk is the boot disk: the image's layers and logs, no state.
const controlDiskGB = 20

// MetadataServer is the address the gateway's container is cut off
// from (but for DNS, which GCE also answers there).
const MetadataServer = "169.254.169.254"

// Control is step 12, control (install.md; gateway.md "Placement",
// SV-89): the VM silo-control in the home region, on Container-Optimized
// OS, with the silod service account, no external IP and no state disk,
// at silo-gateway-ip. It runs two containers of the one image, silod and
// the gateway, which talk over a Unix socket. Only silod can reach the
// metadata server, and so the service account's token: the gateway's
// container has a network namespace of its own whose OUTPUT chain
// rejects 169.254.169.254, put there before the gateway starts. See
// ControlUserData.
//
// With --control-replicas above 1 and --gateway-replicas, silo-control
// stays (it is the front door's VM, and the one the root and admin
// steps reach), and the replicas step builds the MIGs and their NLBs
// around it (replicas.go).
type Control struct{ Env *Env }

// ControlObserved is the VM as GCE has it, and what the plan needs to
// say what it should be.
type ControlObserved struct {
	ComputeEnabled bool `json:"computeEnabled"`
	// The VM's, if Zone isn't "".
	Zone           string            `json:"zone,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	MachineType    string            `json:"machineType,omitempty"`
	NetworkIP      string            `json:"networkIP,omitempty"`
	ServiceAccount string            `json:"serviceAccount,omitempty"`
	External       bool              `json:"external,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	// What the VM should run and where: the image's digest, the
	// gateway's address and the database's connection name ("" while
	// they don't exist).
	Digest string `json:"digest,omitempty"`
	// HarnessDigest is the harness image's, with --agent-sessions.
	HarnessDigest string `json:"harnessDigest,omitempty"`
	GatewayIP     string `json:"gatewayIP,omitempty"`
	DBConnection  string `json:"dbConnection,omitempty"`
	// Replicas are the replica groups (replicas.go).
	Replicas *ReplicasObserved `json:"replicas,omitempty"`
}

func (s *Control) Name() string          { return "control" }
func (s *Control) NewObserved() Observed { return &ControlObserved{} }

func (s *Control) Observe(ctx context.Context) (Observed, error) {
	obs, err := s.observeVM(ctx)
	if err != nil {
		return nil, err
	}
	if obs.Replicas, err = (&Replicas{Env: s.Env}).Observe(ctx, obs); err != nil {
		return nil, err
	}
	return obs, nil
}

// observeVM is silo-control alone.
func (s *Control) observeVM(ctx context.Context) (*ControlObserved, error) {
	obs := &ControlObserved{}
	obs.Digest, _ = s.Env.ImageDigest(ctx) // the image step reports why not
	if s.Env.Config.AgentSessions > 0 {
		obs.HarnessDigest, _ = s.Env.HarnessDigest(ctx)
	}
	_, in, _, err := observeDB(ctx, s.Env)
	if err != nil {
		return nil, err
	}
	if in != nil {
		obs.DBConnection = in.ConnectionName
	}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	obs.ComputeEnabled = true
	cfg := &s.Env.Config
	x := s.Env.Cloud.ComputeExtras
	if a, err := x.GetAddress(ctx, cfg.Project, cfg.HomeRegion(), GatewayAddress); err == nil {
		obs.GatewayIP = a.Address
	} else if !IsNotFound(err) {
		return nil, err
	}
	if obs.Zone, err = x.InstanceZone(ctx, cfg.Project, cfg.HomeRegion(), ControlVM); err != nil || obs.Zone == "" {
		return obs, err
	}
	vm, err := s.Env.Cloud.Compute.GetInstance(ctx, cfg.Project, obs.Zone, ControlVM)
	if err != nil {
		return nil, err
	}
	obs.Labels, obs.MachineType = vm.Labels, gce.LastSegment(vm.MachineType)
	for _, n := range vm.NetworkInterfaces {
		obs.NetworkIP = n.NetworkIP
		obs.External = obs.External || len(n.AccessConfigs) > 0
	}
	for _, a := range vm.ServiceAccounts {
		obs.ServiceAccount = a.Email
	}
	obs.Metadata = map[string]string{}
	if vm.Metadata != nil {
		for _, it := range vm.Metadata.Items {
			switch it.Key {
			case ImageKey, UserDataHashKey:
				obs.Metadata[it.Key] = deref(it.Value)
			case CredentialKeyKey:
				obs.Metadata[it.Key] = "set" // the key itself stays out of the plan
			}
		}
	}
	return obs, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Control) Plan(o Observed) []Change {
	obs := o.(*ControlObserved)
	out := s.planVM(obs)
	if obs.Replicas != nil {
		out = append(out, (&Replicas{Env: s.Env}).Plan(obs.Replicas)...)
	}
	return out
}

func (s *Control) planVM(obs *ControlObserved) []Change {
	cfg := &s.Env.Config
	image := ImageName + "@" + shortDigest(obs.Digest)
	if obs.Zone == "" {
		return []Change{{Action: ActionCreate, Object: ControlVM,
			Detail:   fmt.Sprintf("%s cos-stable, silod and gateway containers (%s), at %s", cfg.ControlMachineType, image, GatewayAddress),
			Estimate: 2 * time.Minute}}
	}
	del := fmt.Sprintf("gcloud compute instances delete %s --zone %s", ControlVM, obs.Zone)
	if obs.Labels[LabelKey] != s.Env.ID {
		return []Change{notOurs(s.Env, ControlVM, "the control VM is install's, and holds silod's service account", del)}
	}
	var diffs []string
	if obs.ServiceAccount != SilodEmail(cfg.Project) {
		diffs = append(diffs, fmt.Sprintf("runs as %q", obs.ServiceAccount))
	}
	if obs.External {
		diffs = append(diffs, "has an external IP")
	}
	if obs.GatewayIP != "" && obs.NetworkIP != obs.GatewayIP {
		diffs = append(diffs, fmt.Sprintf("is at %s, not %s's %s", obs.NetworkIP, GatewayAddress, obs.GatewayIP))
	}
	if len(diffs) > 0 {
		return []Change{{Action: ActionFail, Object: ControlVM, Detail: "differs: it " + strings.Join(diffs, ", "),
			Why: "a VM's service account, access config and address are not changed in place by install",
			Fix: del + ", then install again (the VM holds no state)"}}
	}
	var out []Change
	if obs.MachineType != cfg.ControlMachineType {
		out = append(out, Change{Action: ActionUpdate, Object: ControlVM,
			Detail:   fmt.Sprintf("machine type %s, not %s (stops and starts the VM)", cfg.ControlMachineType, obs.MachineType),
			Estimate: 2 * time.Minute})
	}
	switch {
	case obs.Metadata[UserDataHashKey] != userDataHash(s.userData(obs)):
		out = append(out, Change{Action: ActionUpdate, Object: ControlVM,
			Detail: "boot configuration, " + image + " (resets the VM)", Estimate: 2 * time.Minute})
	case obs.Metadata[ImageKey] != DeployedImage(cfg, obs.Digest):
		out = append(out, Change{Action: ActionUpdate, Object: ControlVM,
			Detail:   fmt.Sprintf("%s, not %s (restarts its containers)", image, shortImage(obs.Metadata[ImageKey])),
			Estimate: time.Minute})
	}
	if cfg.AgentSessions > 0 && obs.Metadata[CredentialKeyKey] == "" {
		out = append(out, Change{Action: ActionUpdate, Object: ControlVM, Detail: CredentialKeyKey + " (generated)", Estimate: 10 * time.Second})
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Object: ControlVM, Detail: obs.Zone + ", " + image}}
	}
	return out
}

func shortDigest(d string) string {
	if len(d) > len("sha256:")+12 {
		return d[:len("sha256:")+12]
	}
	if d == "" {
		return "(digest unknown)"
	}
	return d
}

func shortImage(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ImageName + "@" + shortDigest(ref[i+1:])
	}
	if ref == "" {
		return "none"
	}
	return ref
}

func userDataHash(ud string) string {
	sum := sha256.Sum256([]byte(ud))
	return hex.EncodeToString(sum[:])
}

// userData is the boot configuration obs calls for.
func (s *Control) userData(obs *ControlObserved) string {
	cfg := &s.Env.Config
	return ControlUserData(ControlParams{
		Project: cfg.Project, Region: cfg.HomeRegion(), DBConnection: obs.DBConnection,
		RootMemoryMB: cfg.RootMemoryMB, AddressSpace: cfg.AddressSpace,
		ControlReplicas: cfg.ControlReplicas, GatewayReplicas: cfg.GatewayReplicas,
		AgentSessions: cfg.AgentSessions, HarnessImage: DeployedHarnessImage(cfg, obs.HarnessDigest),
	})
}

// HomeZone is the first zone of the home region that is up: where the
// control and migrate VMs go.
func HomeZone(ctx context.Context, env *Env) (string, error) {
	zones, err := env.Cloud.Compute.ListZones(ctx, env.Config.Project)
	if err != nil {
		return "", err
	}
	var names []string
	for _, z := range zones {
		if gce.LastSegment(z.Region) == env.Config.HomeRegion() && (z.Status == "" || z.Status == "UP") {
			names = append(names, z.Name)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no zone of %s is up", env.Config.HomeRegion())
	}
	sort.Strings(names)
	return names[0], nil
}

// PlanRemove deletes the VM, which holds no state.
func (s *Control) PlanRemove(o Observed) []Change {
	obs := o.(*ControlObserved)
	var out []Change
	if obs.Replicas != nil {
		out = (&Replicas{Env: s.Env}).PlanRemove(obs.Replicas)
	}
	switch {
	case obs.Zone == "":
		out = append(out, Change{Action: ActionOK, Detail: "no VM " + ControlVM})
	case obs.Labels[LabelKey] != s.Env.ID:
		out = append(out, Change{Action: ActionOK, Object: ControlVM, Detail: "is not this install's; left alone"})
	default:
		out = append(out, Change{Action: ActionDelete, Object: ControlVM, Detail: "in " + obs.Zone})
	}
	return out
}

func (s *Control) Apply(ctx context.Context, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	if changes[0].Action == ActionDelete {
		// Uninstall: the replica groups first, then silo-control, which
		// the control NLB's anchor group holds.
		if err := (&Replicas{Env: s.Env}).Apply(ctx, true); err != nil {
			return err
		}
		c, project := s.Env.Cloud.Compute, s.Env.Config.Project
		zone, err := s.Env.Cloud.ComputeExtras.InstanceZone(ctx, project, s.Env.Config.HomeRegion(), ControlVM)
		if err != nil || zone == "" {
			return err
		}
		op, err := c.DeleteInstance(ctx, project, zone, ControlVM)
		if err == nil {
			err = await(ctx, c, project, op)
		}
		if err != nil && !IsNotFound(err) {
			return fmt.Errorf("deleting %s: %w", ControlVM, err)
		}
		return nil
	}
	obs, err := s.observeVM(ctx)
	if err != nil {
		return err
	}
	if p := (StepPlan{Changes: s.planVM(obs)}); p.Changing() {
		if err := s.applyVM(ctx, obs); err != nil {
			return err
		}
	}
	r := &Replicas{Env: s.Env}
	ro, err := r.observe(ctx)
	if err != nil {
		return err
	}
	if p := (StepPlan{Changes: r.Plan(ro)}); p.Changing() {
		return r.Apply(ctx, false)
	}
	return nil
}

// applyVM creates or updates silo-control.
func (s *Control) applyVM(ctx context.Context, obs *ControlObserved) error {
	if obs.Digest == "" {
		if _, err := s.Env.ImageDigest(ctx); err != nil {
			return err
		}
	}
	if obs.GatewayIP == "" {
		return fmt.Errorf("there is no address %s: the address step makes it", GatewayAddress)
	}
	if obs.DBConnection == "" {
		return fmt.Errorf("there is no database instance: the db-instance step makes it")
	}
	if obs.Zone == "" {
		return s.create(ctx, obs)
	}
	return s.update(ctx, obs)
}

// metadata is the VM's metadata: install's keys, over what keep holds.
func (s *Control) metadata(obs *ControlObserved, keep []*compute.MetadataItems) []*compute.MetadataItems {
	cfg := &s.Env.Config
	ud := s.userData(obs)
	want := map[string]string{
		ImageKey:                 DeployedImage(cfg, obs.Digest),
		userDataKey:              ud,
		UserDataHashKey:          userDataHash(ud),
		"google-logging-enabled": "true",
		// Nobody logs in with a project-wide SSH key: the VM holds
		// silod's token.
		"block-project-ssh-keys": "TRUE",
		"enable-oslogin":         "TRUE",
	}
	var out []*compute.MetadataItems
	hasKey := false
	for _, it := range keep {
		if _, ours := want[it.Key]; ours {
			continue
		}
		hasKey = hasKey || it.Key == CredentialKeyKey
		out = append(out, it)
	}
	if cfg.AgentSessions > 0 && !hasKey {
		out = append(out, &compute.MetadataItems{Key: CredentialKeyKey, Value: ptr(newCredentialKey())})
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, &compute.MetadataItems{Key: k, Value: ptr(want[k])})
	}
	return out
}

func newCredentialKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func (s *Control) create(ctx context.Context, obs *ControlObserved) error {
	cfg := &s.Env.Config
	project, region := cfg.Project, cfg.HomeRegion()
	zone, err := HomeZone(ctx, s.Env)
	if err != nil {
		return err
	}
	vm := &compute.Instance{
		Name:        ControlVM,
		Description: owned(s.Env, "silod and the gateway"),
		MachineType: "zones/" + zone + "/machineTypes/" + cfg.ControlMachineType,
		Labels:      map[string]string{LabelKey: s.Env.ID},
		// The boot disk only: silod's state is in Cloud SQL.
		Disks: []*compute.AttachedDisk{{Boot: true, AutoDelete: true,
			InitializeParams: &compute.AttachedDiskInitializeParams{SourceImage: cosImage, DiskSizeGb: controlDiskGB}}},
		// No access configs: no external IP. silo-gateway has NAT and
		// Private Google Access.
		NetworkInterfaces: []*compute.NetworkInterface{{
			Network:    gce.SelfLinkBase + networkPath(project),
			Subnetwork: gce.SelfLinkBase + subnetPath(project, region, GatewaySubnetName),
			NetworkIP:  obs.GatewayIP,
		}},
		ServiceAccounts: []*compute.ServiceAccount{{Email: SilodEmail(project),
			Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}}},
		Metadata:               &compute.Metadata{Items: s.metadata(obs, nil)},
		ShieldedInstanceConfig: &compute.ShieldedInstanceConfig{EnableSecureBoot: true, EnableVtpm: true, EnableIntegrityMonitoring: true},
	}
	op, err := s.Env.Cloud.Compute.InsertInstance(ctx, project, zone, vm, "")
	if err == nil {
		err = await(ctx, s.Env.Cloud.Compute, project, op)
	}
	if err != nil && !IsAlreadyExists(err) {
		return fmt.Errorf("creating %s: %w", ControlVM, err)
	}
	return nil
}

func (s *Control) update(ctx context.Context, obs *ControlObserved) error {
	c := s.Env.Cloud.Compute
	cfg := &s.Env.Config
	project, zone := cfg.Project, obs.Zone
	do := func(what string, op *compute.Operation, err error) error {
		if err == nil {
			err = await(ctx, c, project, op)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", what, ControlVM, err)
		}
		return nil
	}
	if obs.MachineType != cfg.ControlMachineType {
		op, err := c.StopInstance(ctx, project, zone, ControlVM, false)
		if err := do("stopping", op, err); err != nil {
			return err
		}
		op, err = c.SetMachineType(ctx, project, zone, ControlVM, "zones/"+zone+"/machineTypes/"+cfg.ControlMachineType)
		if err := do("resizing", op, err); err != nil {
			return err
		}
	}
	vm, err := c.GetInstance(ctx, project, zone, ControlVM)
	if err != nil {
		return err
	}
	var items []*compute.MetadataItems
	fp := ""
	if vm.Metadata != nil {
		items, fp = vm.Metadata.Items, vm.Metadata.Fingerprint
	}
	reset := obs.Metadata[UserDataHashKey] != userDataHash(s.userData(obs))
	op, err := c.SetInstanceMetadata(ctx, project, zone, ControlVM, &compute.Metadata{Items: s.metadata(obs, items), Fingerprint: fp})
	if err := do("setting the metadata of", op, err); err != nil {
		return err
	}
	switch {
	case vm.Status == "TERMINATED" || vm.Status == "STOPPED":
		op, err := c.StartInstance(ctx, project, zone, ControlVM)
		return do("starting", op, err)
	case reset:
		// The boot configuration is read at boot.
		op, err := c.ResetInstance(ctx, project, zone, ControlVM)
		return do("resetting", op, err)
	}
	// An image change alone: the VM's silo-image-watch unit sees the
	// metadata change and restarts both containers on the new image.
	return nil
}

// ControlParams are what the control VM's boot configuration depends
// on.
type ControlParams struct {
	Project, Region string
	// DBConnection is the instance's <project>:<region>:<name>.
	DBConnection string
	RootMemoryMB int64
	// AddressSpace is --address-space, which the gateway's container
	// network must stay out of.
	AddressSpace netip.Prefix
	// ControlReplicas and GatewayReplicas are the flags': silod's
	// replica count, and whether it serves gateway replicas over mTLS
	// and issues their certificates.
	ControlReplicas, GatewayReplicas int
	// AgentSessions is --agent-sessions, and HarnessImage the harness
	// image the sessions run, by digest: with sessions, the VM also
	// runs the session launcher, silod sessiond.
	AgentSessions int
	HarnessImage  string
}

// SessiondSocket is the session launcher's socket, which only silod's
// user may open.
const SessiondSocket = "/run/silo/sessiond.sock"

// gatewayNetworks are the container networks the gateway's namespace
// may be on, in order: the first outside --address-space is used.
var gatewayNetworks = []string{"172.31.255.248/29", "192.168.255.248/29", "100.127.255.248/29"}

// GatewayNetwork is the docker network the gateway's namespace is on,
// and the gateway's address in it.
func GatewayNetwork(space netip.Prefix) (netip.Prefix, netip.Addr) {
	for _, n := range gatewayNetworks {
		p := netip.MustParsePrefix(n)
		if !space.Overlaps(p) {
			return p, p.Addr().Next().Next()
		}
	}
	p := netip.MustParsePrefix(gatewayNetworks[0])
	return p, p.Addr().Next().Next()
}

// gatewayPorts are the gateway's TCP ports, forwarded from the VM's
// address into the gateway's namespace with the caller's source address
// kept, which the gateway's origin identity reads.
func gatewayPorts() string {
	return fmt.Sprintf("%d,%d,%d,%d:%d", gateway.HTTPPort, gateway.APIPort, gateway.EgressPort, gateway.FirstTCPExport, gateway.LastTCPExport)
}

// ControlUserData is the control VM's cloud-init configuration, which
// Container-Optimized OS applies at every boot. It runs three systemd
// units:
//
//   - silod.service: silod serve, as user SilodUID, on the host's
//     network, so that it reaches the metadata server for the service
//     account's token (Cloud SQL IAM login, GCE). It serves the front
//     door on port 8443 and the gateway socket /run/silo/gateway.sock,
//     mode 0660 to group GatewayGroup.
//   - silo-gateway.service: first a silod pause container that holds a
//     network namespace of the gateway's own; then, in that namespace,
//     iptables rules that reject everything to 169.254.169.254 but DNS;
//     only then the gateway, joining that namespace as user GatewayUID
//     with every capability dropped, so it can't remove the rules. No
//     process in the gateway's container ever runs with a route to the
//     metadata server, so none can fetch the service account's token,
//     and an egress proxy request for 169.254.169.254 is refused by the
//     same rule. The gateway's ports are DNATed from the VM's address
//     into the namespace, which keeps callers' source addresses.
//   - silo-image-watch.service: waits on the silo-image metadata key,
//     and restarts both units when install changes it.
//
// The units read the image from the metadata, not from this text, so
// that an upgrade changes one key and resets nothing.
func ControlUserData(p ControlParams) string {
	net, gwIP := GatewayNetwork(p.AddressSpace)
	registry := p.Region + "-docker.pkg.dev"
	silodArgs := []string{"serve",
		"--host-project", p.Project,
		"--fence-project", p.Project,
		"--db-instance", p.DBConnection,
		"--db-name", DBName,
		"--db-user", DBUser(p.Project),
		"--gateway-socket", "/run/silo/gateway.sock",
		"--gateway-socket-group", strconv.Itoa(GatewayGroup),
		"--front-door-listen", fmt.Sprintf(":%d", 8443),
	}
	if p.RootMemoryMB > 0 {
		silodArgs = append(silodArgs, "--root-memory", strconv.FormatInt(p.RootMemoryMB, 10))
	}
	if p.ControlReplicas > 1 {
		silodArgs = append(silodArgs, "--control-replicas", strconv.Itoa(p.ControlReplicas))
	}
	if p.ControlReplicas > 1 || p.GatewayReplicas > 0 {
		// A health-checked MIG: the fence lets Google's checkers in.
		silodArgs = append(silodArgs, "--gateway-replicas")
	}
	silodSelf := ""
	if p.AgentSessions > 0 {
		// Harness sessions, through the launcher; silod counts its own
		// slots from its VM's size, up to the fleet's count.
		silodArgs = append(silodArgs, "--sessiond-socket", SessiondSocket, "--agent-sessions", strconv.Itoa(p.AgentSessions))
		// Its heartbeat names this VM's gateway, which another control
		// VM's gateway redirects its sessions' tool channels to: the
		// VM's own address, as the gateway's --host-address.
		silodSelf = ` --gateway-address "$(md network-interfaces/0/ip)"`
	}
	silodPorts := "8443"
	if p.GatewayReplicas > 0 {
		silodArgs = append(silodArgs, "--gateway-tls-listen", fmt.Sprintf(":%d", gwtls.Port), "--home-region", p.Region,
			"--gateway-registry", fmt.Sprintf("%s-docker.pkg.dev/%s/%s/%s", p.Region, p.Project, RepositoryName, ImageName))
		silodPorts += "," + strconv.Itoa(gwtls.Port)
	}
	quoted := make([]string, len(silodArgs))
	for i, a := range silodArgs {
		quoted[i] = shellQuote(a)
	}
	r := strings.NewReplacer(
		"@REGISTRY@", registry,
		"@SILOD_ARGS@", strings.Join(quoted, " ")+silodSelf,
		"@SILOD_UID@", strconv.Itoa(SilodUID),
		"@GATEWAY_UID@", strconv.Itoa(GatewayUID),
		"@GATEWAY_GID@", strconv.Itoa(GatewayGroup),
		"@GW_NET@", net.String(),
		"@GW_IP@", gwIP.String(),
		"@GW_PORTS@", gatewayPorts(),
		"@METADATA@", MetadataServer,
		"@IMAGE_KEY@", ImageKey,
		"@SILOD_PORTS@", silodPorts,
		"@SESSIOND_SOCKET@", SessiondSocket,
		"@HARNESS_IMAGE@", shellQuote(p.HarnessImage),
	)
	ud := userDataTemplate
	if p.AgentSessions > 0 {
		ud = strings.Replace(ud, "runcmd:\n", sessiondUserData+"runcmd:\n", 1)
		ud = strings.Replace(ud, "- systemctl start silod.service silo-gateway.service silo-image-watch.service",
			"- systemctl start silo-sessiond.service silod.service silo-gateway.service silo-image-watch.service", 1)
		ud = strings.Replace(ud, "systemctl restart silod.service silo-gateway.service", "systemctl restart silo-sessiond.service silod.service silo-gateway.service", 1)
	}
	return r.Replace(ud)
}

// sessiondUserData is the session launcher's part of the control VM's
// boot configuration, with --agent-sessions (agents.md, "Isolation on
// the control VM", SV-118). silo-sessiond.service runs silod sessiond
// as root, the one container on the VM with the container runtime's
// socket, with no network and only the capabilities it needs to hand
// sessions their own socket and private directory (CHOWN, FOWNER,
// DAC_OVERRIDE). It serves silod alone, on a socket of mode 0600
// owned by silod's uid, and refuses any session spec that loosens a
// session's isolation (package harness). silod itself still runs
// without the runtime's socket, as its own user.
const sessiondUserData = `- path: /etc/silo/sessiond.sh
  permissions: "0644"
  content: |
    . /etc/silo/common.sh
    mkdir -p /run/silo /var/lib/silo/sessions
    chmod 0711 /var/lib/silo/sessions
    harness=@HARNESS_IMAGE@
    docker pull "$harness" >/dev/null
    docker rm -f silo-sessiond >/dev/null 2>&1 || true
    exec docker run --rm --name silo-sessiond --network none --user 0:0 \
      --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
      --security-opt no-new-privileges \
      -v /var/run/docker.sock:/var/run/docker.sock \
      -v /run/silo:/run/silo -v /var/lib/silo/sessions:/var/lib/silo/sessions \
      "$(image)" sessiond --socket @SESSIOND_SOCKET@ --owner @SILOD_UID@ --harness-image "$harness"
- path: /etc/systemd/system/silo-sessiond.service
  permissions: "0644"
  content: |
    [Unit]
    Description=the silo session launcher
    Wants=network-online.target
    After=network-online.target docker.service
    Requires=docker.service
    [Service]
    ExecStart=/bin/bash /etc/silo/sessiond.sh
    ExecStop=/usr/bin/docker stop silo-sessiond
    Restart=always
    RestartSec=5
`

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

const userDataTemplate = `#cloud-config
# silo-control (silo install, step 12). Applied at every boot.
write_files:
- path: /etc/silo/common.sh
  permissions: "0644"
  content: |
    set -euo pipefail
    export HOME=/var/lib/silo
    mkdir -p "$HOME"
    md() { curl -sSf -H 'Metadata-Flavor: Google' "http://@METADATA@/computeMetadata/v1/instance/$1"; }
    image() { md attributes/@IMAGE_KEY@; }
    /usr/bin/docker-credential-gcr configure-docker --registries @REGISTRY@ >/dev/null
- path: /etc/silo/silod.sh
  permissions: "0644"
  content: |
    . /etc/silo/common.sh
    mkdir -p /run/silo
    chown @SILOD_UID@:@GATEWAY_GID@ /run/silo
    chmod 0750 /run/silo
    # The front door: IAP's TCP forwarding reaches port 8443; with
    # gateway replicas, they reach silod over mTLS on 8444.
    iptables -w -C INPUT -p tcp -m multiport --dports @SILOD_PORTS@ -j ACCEPT 2>/dev/null ||
      iptables -w -A INPUT -p tcp -m multiport --dports @SILOD_PORTS@ -j ACCEPT
    docker rm -f silod >/dev/null 2>&1 || true
    # silod alone has the host's network, and so the metadata server.
    exec docker run --rm --name silod --network host \
      --user @SILOD_UID@:@SILOD_UID@ --group-add @GATEWAY_GID@ \
      --cap-drop ALL --security-opt no-new-privileges \
      -v /run/silo:/run/silo \
      "$(image)" @SILOD_ARGS@
- path: /etc/silo/gateway.sh
  permissions: "0644"
  content: |
    . /etc/silo/common.sh
    img="$(image)"
    docker rm -f silo-gateway silo-gateway-net >/dev/null 2>&1 || true
    docker network inspect silo-gw >/dev/null 2>&1 || docker network create --subnet @GW_NET@ silo-gw >/dev/null
    # A namespace of the gateway's own, held by a container that does
    # nothing, so that its rules are in place before the gateway runs.
    docker run -d --name silo-gateway-net --network silo-gw --ip @GW_IP@ \
      --user @GATEWAY_UID@:@GATEWAY_GID@ --cap-drop ALL --security-opt no-new-privileges \
      "$img" pause >/dev/null
    pid="$(docker inspect -f '{{.State.Pid}}' silo-gateway-net)"
    ns() { nsenter -t "$pid" -n "$@"; }
    # No route to the metadata server, and so no token: only DNS, which
    # GCE also answers at that address, gets through.
    ns iptables -w -A OUTPUT -d @METADATA@ -p udp --dport 53 -j ACCEPT
    ns iptables -w -A OUTPUT -d @METADATA@ -p tcp --dport 53 -j ACCEPT
    ns iptables -w -A OUTPUT -d @METADATA@ -j REJECT
    ns iptables -w -C OUTPUT -d @METADATA@ -j REJECT
    ns ip6tables -w -A OUTPUT -d fd20:ce::254 -j REJECT 2>/dev/null || true
    # The gateway's ports, from any of this VM's own addresses into the
    # namespace, callers' source addresses kept: the VM's, and an
    # internal passthrough NLB's, whose packets arrive addressed to the
    # forwarding rule's IP, which the guest agent makes local.
    self="$(md network-interfaces/0/ip)"
    iptables -w -t nat -C PREROUTING -m addrtype --dst-type LOCAL -p tcp -m multiport --dports @GW_PORTS@ -j DNAT --to-destination @GW_IP@ 2>/dev/null ||
      iptables -w -t nat -A PREROUTING -m addrtype --dst-type LOCAL -p tcp -m multiport --dports @GW_PORTS@ -j DNAT --to-destination @GW_IP@
    iptables -w -C DOCKER-USER -d @GW_IP@ -p tcp -m multiport --dports @GW_PORTS@ -j ACCEPT 2>/dev/null ||
      iptables -w -I DOCKER-USER -d @GW_IP@ -p tcp -m multiport --dports @GW_PORTS@ -j ACCEPT
    exec docker run --rm --name silo-gateway --network container:silo-gateway-net \
      --user @GATEWAY_UID@:@GATEWAY_GID@ --cap-drop ALL --security-opt no-new-privileges \
      -v /run/silo:/run/silo \
      "$img" gateway --silod-socket /run/silo/gateway.sock --host-address "$self"
- path: /etc/silo/watch.sh
  permissions: "0644"
  content: |
    . /etc/silo/common.sh
    cur="$(image)"
    while true; do
      new="$(md "attributes/@IMAGE_KEY@?wait_for_change=true&timeout_sec=600")" || { sleep 10; continue; }
      if [ -n "$new" ] && [ "$new" != "$cur" ]; then
        cur="$new"
        systemctl restart silod.service silo-gateway.service
      fi
    done
- path: /etc/systemd/system/silod.service
  permissions: "0644"
  content: |
    [Unit]
    Description=silod
    Wants=network-online.target
    After=network-online.target docker.service
    Requires=docker.service
    [Service]
    ExecStart=/bin/bash /etc/silo/silod.sh
    ExecStop=/usr/bin/docker stop silod
    Restart=always
    RestartSec=5
- path: /etc/systemd/system/silo-gateway.service
  permissions: "0644"
  content: |
    [Unit]
    Description=the silo gateway
    Wants=network-online.target
    After=network-online.target docker.service silod.service
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
    Description=restart silod and the gateway when silo-image changes
    After=silod.service silo-gateway.service
    [Service]
    ExecStart=/bin/bash /etc/silo/watch.sh
    Restart=always
    RestartSec=10
runcmd:
- systemctl daemon-reload
- systemctl start silod.service silo-gateway.service silo-image-watch.service
`
