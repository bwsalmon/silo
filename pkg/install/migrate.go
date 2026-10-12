package install

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/store/migrations"
	compute "google.golang.org/api/compute/v1"
)

// The migrate step's VM.
const (
	MigrateVM = "silo-migrate"
	// MigrateAttribute is the guest attribute silod migrate reports in
	// (cmd/silod's MigrateAttribute): "ok <version>" or "error <why>".
	MigrateAttribute = "silo/migrate"
	migrateMachine   = "e2-medium"
	cosImage         = "projects/cos-cloud/global/images/family/cos-stable"
)

// MigrateTimeout is how long the migrate step waits for the VM's
// result. PollInterval is how often it looks; tests shorten it.
var (
	MigrateTimeout = 10 * time.Minute
	PollInterval   = 5 * time.Second
)

// SchemaVersion is the schema this release's silod migrates to: the
// newest migration it carries.
func SchemaVersion() int {
	ms, err := migrations.Load("postgres")
	if err != nil {
		panic(err) // the embedded files are checked by their own tests
	}
	return len(ms)
}

// Migrate is step 11c, migrate (state.md "Schema migrations and
// upgrades", SV-53): the instance has no public IP, so install can't
// reach the database. A short-lived VM, silo-migrate, in the gateway
// subnet with the silod service account and no external IP, runs the
// image's silod migrate --to <version> and reports the result as a
// guest attribute; install then deletes the VM, and records the
// version as the instance's silo-schema label for the next run's plan.
// On an instance install has just adopted, silod migrate also sets
// reconcile to report-only (SV-54).
type Migrate struct{ Env *Env }

// MigrateObserved is the schema the instance's label records, whether
// it is an adopted instance still to be marked restored, and any VM
// left behind by an interrupted run.
type MigrateObserved struct {
	Instance string `json:"instance,omitempty"`
	State    string `json:"state,omitempty"`
	// Schema is the silo-schema label, -1 if there is none.
	Schema   int  `json:"schema"`
	Restored bool `json:"restored,omitempty"`
	// VMZone is the zone of a silo-migrate VM, "" if there is none.
	VMZone string `json:"vmZone,omitempty"`
}

func (s *Migrate) Name() string          { return "migrate" }
func (s *Migrate) homeRegion() string    { return s.Env.Config.HomeRegion() }
func (s *Migrate) NewObserved() Observed { return &MigrateObserved{} }

func (s *Migrate) Observe(ctx context.Context) (Observed, error) {
	obs := &MigrateObserved{Schema: -1}
	_, in, _, err := observeDB(ctx, s.Env)
	if err != nil {
		return nil, err
	}
	if in != nil {
		obs.Instance, obs.State = in.Name, in.State
		if v, err := strconv.Atoi(in.Labels[SchemaLabel]); err == nil && s.Env.ours(in) {
			obs.Schema = v
		}
		obs.Restored = in.Labels[RestoredLabel] != "" || !s.Env.ours(in)
	}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	if obs.VMZone, err = s.Env.Cloud.ComputeExtras.InstanceZone(ctx, s.Env.Config.Project, s.Env.Config.HomeRegion(), MigrateVM); err != nil {
		return nil, err
	}
	return obs, nil
}

func (s *Migrate) Plan(o Observed) []Change {
	obs := o.(*MigrateObserved)
	to := SchemaVersion()
	from := "0"
	if obs.Schema >= 0 {
		from = strconv.Itoa(obs.Schema)
	} else if obs.Instance != "" && obs.State == "RUNNABLE" {
		from = "?"
	}
	var out []Change
	if obs.VMZone != "" {
		out = append(out, Change{Action: ActionDelete, Object: MigrateVM, Detail: "left by an interrupted run, in " + obs.VMZone,
			Estimate: 30 * time.Second})
	}
	switch {
	case obs.Schema == to && !obs.Restored:
		if len(out) == 0 {
			return []Change{{Action: ActionOK, Detail: "schema " + strconv.Itoa(to)}}
		}
	case obs.Schema > to && !obs.Restored:
		// A rollback: the newer release's expand steps leave a schema
		// this release runs on, and migrations only go forward.
		if len(out) == 0 {
			return []Change{{Action: ActionOK, Detail: fmt.Sprintf("schema %d, newer than this release's %d", obs.Schema, to)}}
		}
	default:
		detail := fmt.Sprintf("schema %s -> %d", from, to)
		if obs.Restored {
			detail += "; reconcile report-only (adopted instance)"
		}
		out = append(out, Change{Action: ActionApply, Detail: detail, Estimate: time.Minute})
	}
	return out
}

// PlanRemove deletes a silo-migrate VM an interrupted run left.
func (s *Migrate) PlanRemove(o Observed) []Change {
	obs := o.(*MigrateObserved)
	if obs.VMZone == "" {
		return []Change{{Action: ActionOK, Detail: "no " + MigrateVM}}
	}
	return []Change{{Action: ActionDelete, Object: MigrateVM, Detail: "in " + obs.VMZone}}
}

// MigrateArgs are silod migrate's arguments on the VM.
func MigrateArgs(project string, in *SQLInstance, to int, restored bool) []string {
	args := []string{"migrate", "--to", strconv.Itoa(to), "--report-guest-attribute",
		"--db-instance", in.ConnectionName, "--db-name", DBName, "--db-user", DBUser(project)}
	if restored {
		args = append(args, "--reconcile-report-only")
	}
	return args
}

// containerDeclaration is the COS konlet declaration running image with
// args once.
func containerDeclaration(image string, args []string) string {
	var b strings.Builder
	b.WriteString("spec:\n  containers:\n  - name: silod-migrate\n    image: " + image + "\n    args:\n")
	for _, a := range args {
		b.WriteString("    - " + strconv.Quote(a) + "\n")
	}
	b.WriteString("  restartPolicy: Never\n")
	return b.String()
}

func (s *Migrate) Apply(ctx context.Context, changes []Change) error {
	c, x := s.Env.Cloud.Compute, s.Env.Cloud.ComputeExtras
	project := s.Env.Config.Project
	deleteVM := func(zone string) error {
		op, err := c.DeleteInstance(ctx, project, zone, MigrateVM)
		if IsNotFound(err) {
			return nil
		}
		if err == nil {
			err = await(ctx, c, project, op)
		}
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, ch := range changes {
		switch ch.Action {
		case ActionDelete:
			zone, err := x.InstanceZone(ctx, project, s.Env.Config.HomeRegion(), MigrateVM)
			if err != nil {
				return err
			}
			if zone != "" {
				if err := deleteVM(zone); err != nil {
					return fmt.Errorf("deleting %s: %w", MigrateVM, err)
				}
			}
		case ActionApply:
			if err := s.migrate(ctx, deleteVM); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Migrate) migrate(ctx context.Context, deleteVM func(zone string) error) error {
	c, sql := s.Env.Cloud.Compute, s.Env.Cloud.SQLAdmin
	cfg := &s.Env.Config
	project, region := cfg.Project, cfg.HomeRegion()
	_, in, _, err := observeDB(ctx, s.Env)
	if err != nil {
		return err
	}
	if in == nil {
		return fmt.Errorf("no database instance: the db-instance step creates it")
	}
	if in, err = sql.WaitInstance(ctx, project, in.Name); err != nil {
		return err
	}
	restored := in.Labels[RestoredLabel] != ""
	to := SchemaVersion()
	zone, err := HomeZone(ctx, s.Env)
	if err != nil {
		return err
	}
	digest, err := s.Env.ImageDigest(ctx)
	if err != nil {
		return err
	}
	vm := &compute.Instance{
		Name:        MigrateVM,
		Description: owned(s.Env, "runs silod migrate, then is deleted"),
		MachineType: "zones/" + zone + "/machineTypes/" + migrateMachine,
		Labels:      map[string]string{LabelKey: s.Env.ID},
		Disks: []*compute.AttachedDisk{{Boot: true, AutoDelete: true,
			InitializeParams: &compute.AttachedDiskInitializeParams{SourceImage: cosImage, DiskSizeGb: 10}}},
		// No access configs: no external IP. The gateway subnet has
		// Private Google Access and NAT, for the image pull and the
		// Cloud SQL Admin API the connector calls.
		NetworkInterfaces: []*compute.NetworkInterface{{
			Network:    gce.SelfLinkBase + networkPath(project),
			Subnetwork: gce.SelfLinkBase + subnetPath(project, region, GatewaySubnetName),
		}},
		ServiceAccounts: []*compute.ServiceAccount{{Email: SilodEmail(project),
			Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}}},
		Metadata: &compute.Metadata{Items: []*compute.MetadataItems{
			{Key: "gce-container-declaration", Value: ptr(containerDeclaration(DeployedImage(cfg, digest), MigrateArgs(project, in, to, restored)))},
			{Key: "enable-guest-attributes", Value: ptr("TRUE")},
			{Key: "google-logging-enabled", Value: ptr("true")},
		}},
		ShieldedInstanceConfig: &compute.ShieldedInstanceConfig{EnableSecureBoot: true, EnableVtpm: true, EnableIntegrityMonitoring: true},
	}
	op, err := c.InsertInstance(ctx, project, zone, vm, "")
	if err == nil {
		err = await(ctx, c, project, op)
	}
	if err != nil && !IsAlreadyExists(err) {
		return fmt.Errorf("creating %s: %w", MigrateVM, err)
	}
	result, waitErr := s.await(ctx, zone)
	// The VM goes whatever happened: it is created afresh on the next
	// run.
	if err := deleteVM(zone); err != nil {
		return errorsJoin(waitErr, fmt.Errorf("deleting %s: %w", MigrateVM, err))
	}
	if waitErr != nil {
		return waitErr
	}
	if msg, failed := strings.CutPrefix(result, "error "); failed {
		return fmt.Errorf("silod migrate --to %d failed on %s: %s", to, MigrateVM, msg)
	}
	if result != fmt.Sprintf("ok %d", to) {
		return fmt.Errorf("silod migrate on %s reported %q, want ok %d", MigrateVM, result, to)
	}
	in, err = sql.GetInstance(ctx, project, in.Name)
	if err != nil {
		return err
	}
	in.Labels = merge(in.Labels, map[string]string{SchemaLabel: strconv.Itoa(to)})
	delete(in.Labels, RestoredLabel)
	return sql.PatchInstance(ctx, project, in)
}

// await polls the VM's guest attribute for silod migrate's result.
func (s *Migrate) await(ctx context.Context, zone string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, MigrateTimeout)
	defer cancel()
	for {
		v, err := s.Env.Cloud.ComputeExtras.GuestAttribute(ctx, s.Env.Config.Project, zone, MigrateVM, MigrateAttribute)
		switch {
		case err == nil && v != "":
			return v, nil
		case err != nil && !IsNotFound(err):
			return "", fmt.Errorf("reading %s's result: %w", MigrateVM, err)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%s reported no result in %s (its serial console and Cloud Logging say why)", MigrateVM, MigrateTimeout)
		case <-time.After(PollInterval):
		}
	}
}

func ptr(s string) *string { return &s }

func errorsJoin(a, b error) error {
	if a == nil {
		return b
	}
	return fmt.Errorf("%w; %w", a, b)
}
