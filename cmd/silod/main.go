// Command silod is Silo's control plane: one binary with three
// commands.
//
//	silod serve     serve the silo/v1 fence API (docs/silo/fence-api.md)
//	                and, given --host-project, the compute/v1 facade
//	                (docs/silo/gcp-backend.md); --front-door-listen adds
//	                the front door (docs/silo/gateway.md, SV-88);
//	                --backend=local serves one machine's local backend
//	                (docs/silo/local-backend.md) on --data-dir's SQLite
//	                store, with the fence in silo-nsd's silo-net
//	silod migrate   apply schema migrations (docs/silo/state.md, SV-53)
//	silod gateway   the gateway (docs/silo/gateway.md): the API port,
//	                port 80 and the egress proxy, in front of silod's
//	                gateway socket
//	silod pause     do nothing until stopped: on the control VM it holds
//	                the gateway's network namespace, so that the rule
//	                cutting it off from the metadata server is in place
//	                before the gateway starts (install's control step)
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/backend"
	"github.com/bwsalmon/silo/pkg/backend/gcp"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/gceclient"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/gcefence"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/rootcap"
	"github.com/bwsalmon/silo/pkg/backend/local"
	"github.com/bwsalmon/silo/pkg/backend/local/datadir"
	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	_ "github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/gvisor"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	facade "github.com/bwsalmon/silo/pkg/facade/gcp/gce"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/frontdoor"
	"github.com/bwsalmon/silo/pkg/gateway"
	"github.com/bwsalmon/silo/pkg/gitserve"
	"github.com/bwsalmon/silo/pkg/gwtls"
	"github.com/bwsalmon/silo/pkg/harness"
	"github.com/bwsalmon/silo/pkg/leader"
	"github.com/bwsalmon/silo/pkg/mirror"
	"github.com/bwsalmon/silo/pkg/modelproxy"
	"github.com/bwsalmon/silo/pkg/reconcile"
	"github.com/bwsalmon/silo/pkg/session"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/migrations"
	"github.com/bwsalmon/silo/pkg/store/postgres"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/toolchan"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
)

const usage = `usage: silod <command> [flags]

commands:
  serve     serve the silo/v1 fence API
  migrate   apply schema migrations
  gateway   run the gateway
  pause     hold a network namespace until stopped
  sessiond  launch agents' harness session containers for silod
  nsd       silo-nsd, the local backend's rootless namespace holder
  runsc     runsc inside a container's user namespace, for the sandbox class's shim
  runtime-log
            a sandbox-class container's log copier
  runtime-remap
            remap a container's snapshot to its user namespace, in the holder
  containerd
            silo-containerd: containerd inside silo-nsd's namespaces
  buildkitd silo-buildkitd: buildkitd inside silo-nsd's namespaces
  harness-init
            a session container's entrypoint: forward the session
            socket to the loopback, and run the harness
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "migrate":
		err = migrate(ctx, os.Args[2:])
	case "gateway":
		err = runGateway(ctx, os.Args[2:])
	case "pause":
		<-ctx.Done()
	case "sessiond":
		err = runSessiond(ctx, os.Args[2:])
	case "nsd":
		// silo-nsd, the local backend's namespace holder.
		var exe string
		if exe, err = os.Executable(); err == nil {
			err = nsd.Main(ctx, exe, os.Args[2:])
		}
	case "runsc":
		// The sandbox class's runsc, as its containerd shim calls it:
		// runsc started inside the container's user namespace.
		var exe string
		if exe, err = os.Executable(); err == nil {
			err = rootless.RunscMain(exe, os.Args[2:])
		}
	case gvisor.LogArg:
		// A sandbox-class container's log copier, which outlives silod.
		err = gvisor.LogMain(os.Args[2:])
	case gvisor.RemapArg:
		// Remapping a container's snapshot, run inside the holder.
		err = gvisor.RemapMain(os.Args[2:])
	case "containerd", "buildkitd":
		// silo-containerd and silo-buildkitd, which replace this
		// process with the runtime inside the holder's namespaces.
		var exe string
		if exe, err = os.Executable(); err == nil {
			if os.Args[1] == "containerd" {
				err = rootless.ContainerdMain(ctx, exe, os.Args[2:])
			} else {
				err = rootless.BuildkitdMain(ctx, exe, os.Args[2:])
			}
		}
	case gitserve.HookArg:
		// The git service's pre-receive hook, which receive-pack runs.
		os.Exit(gitserve.HookMain(os.Args[2:], os.Stdin, os.Stderr))
	case "harness-init":
		var code int
		code, err = harnessInit(ctx, os.Args[2:])
		if err == nil {
			os.Exit(code)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("silod %s: %v", os.Args[1], err)
	}
}

// release is this binary's release, set at build time with
// -ldflags "-X main.release=v1.2.3". Replicas report it in their
// heartbeat.
var release = "dev"

// storeFlags are the flags that say which database to open.
type storeFlags struct {
	spec, url              string
	instance, name, dbUser string
	dataDir                string
}

func (f *storeFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.spec, "store", "postgres", "the store: postgres, or sqlite:<path>")
	fs.StringVar(&f.url, "database-url", "", "a Postgres connection string, instead of Cloud SQL (default $SILOD_DATABASE_URL)")
	fs.StringVar(&f.instance, "db-instance", "", "the Cloud SQL instance, <project>:<region>:<instance> (default $SILOD_DB_INSTANCE)")
	fs.StringVar(&f.name, "db-name", "silo", "the Cloud SQL database")
	fs.StringVar(&f.dbUser, "db-user", "", "the IAM database user, silod@<project>.iam (default $SILOD_DB_USER)")
	fs.StringVar(&f.dataDir, "data-dir", "", "the local backend's data directory, which holds all of silod's persistent state; the store is its state/silo.db")
}

// openDataDir opens --data-dir, if it was given, and points the store
// at the data directory's own: a --store naming another is refused, as
// nothing persistent is kept outside the data directory
// (local-rootless-storage.md call L35).
func (f *storeFlags) openDataDir(fs *flag.FlagSet) (*datadir.Dir, error) {
	if f.dataDir == "" {
		return nil, nil
	}
	dd, err := datadir.Open(f.dataDir)
	if err != nil {
		return nil, err
	}
	explicit := false
	fs.Visit(func(fl *flag.Flag) { explicit = explicit || fl.Name == "store" })
	if explicit && f.spec != dd.StoreSpec() {
		return nil, fmt.Errorf("--store=%s with --data-dir: the store is the data directory's, %s", f.spec, dd.StoreSpec())
	}
	f.spec = dd.StoreSpec()
	return dd, nil
}

// openStore opens the --store database. Postgres is a Cloud SQL
// instance, reached through the Cloud SQL Go connector with IAM login
// and Private Service Connect (state.md "Authentication", SV-51), or,
// for tests and development, a plain connection string.
func openStore(ctx context.Context, f storeFlags) (store.Database, store.Spec, error) {
	sp, err := store.ParseSpec(f.spec)
	if err != nil {
		return nil, sp, err
	}
	if sp.Backend == "sqlite" {
		db, err := sqlite.Open(sp.Path)
		return db, sp, err
	}
	env := func(v *string, name string) {
		if *v == "" {
			*v = os.Getenv(name)
		}
	}
	env(&f.url, "SILOD_DATABASE_URL")
	env(&f.instance, "SILOD_DB_INSTANCE")
	env(&f.dbUser, "SILOD_DB_USER")
	switch {
	case f.url != "" && f.instance != "":
		return nil, sp, errors.New("--store=postgres: give --database-url or --db-instance, not both")
	case f.url != "":
		db, err := postgres.Open(ctx, f.url)
		return db, sp, err
	case f.instance != "":
		db, err := postgres.OpenCloudSQL(ctx, postgres.CloudSQL{Instance: f.instance, Database: f.name, User: f.dbUser})
		return db, sp, err
	}
	return nil, sp, errors.New("--store=postgres needs --db-instance (or $SILOD_DB_INSTANCE), or --database-url")
}

func migrate(ctx context.Context, args []string) (err error) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	var sf storeFlags
	sf.register(fs)
	to := fs.Int("to", 0, "the version to migrate to (default the newest this binary carries)")
	reportOnly := fs.Bool("reconcile-report-only", false, "set reconcile to report-only: the database was restored from a backup")
	guestAttr := fs.Bool("report-guest-attribute", false, "report the result as the guest attribute "+MigrateAttribute+", for install's migrate VM")
	fs.Parse(args)
	if _, err := sf.openDataDir(fs); err != nil {
		return err
	}
	if *guestAttr {
		defer func() { reportGuestAttribute(ctx, MigrateAttribute, migrateResult(*to, err)) }()
	}
	db, _, err := openStore(ctx, sf)
	if err != nil {
		return err
	}
	defer db.Close()
	v := *to
	if v == 0 {
		v = db.Latest()
		*to = v
	}
	if err := db.Migrate(ctx, v); err != nil {
		return err
	}
	if *reportOnly {
		if err := store.SetSetting(ctx, db, store.SettingReconcile, store.ReconcileReportOnly); err != nil {
			return err
		}
		log.Printf("reconcile set to report-only")
	}
	log.Printf("schema at version %d", v)
	return nil
}

// MigrateAttribute is the guest attribute, namespace/key, the migrate
// VM reports silod migrate's result in: "ok <version>", or
// "error <message>".
const MigrateAttribute = "silo/migrate"

func migrateResult(to int, err error) string {
	if err != nil {
		return "error " + err.Error()
	}
	return fmt.Sprintf("ok %d", to)
}

// metadataHost is the metadata server; a test points it elsewhere.
var metadataHost = "http://metadata.google.internal"

// credentialKeyAttribute is install's silo-credential-key, in the
// control VMs' metadata (install.CredentialKeyKey).
const credentialKeyAttribute = "silo-credential-key"

// credentialKey is the key the agent credentials are sealed with: from
// file, or, on a control VM that runs sessions, from the metadata
// server, which only silod can reach (agents.md, §6 and "Isolation on
// the control VM"). None leaves credentials unusable, not silod.
func credentialKey(ctx context.Context, file string, fromMetadata bool) ([]byte, error) {
	var text string
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		text = string(b)
	case fromMetadata:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			metadataHost+"/computeMetadata/v1/instance/attributes/"+credentialKeyAttribute, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			log.Printf("agent credentials: reading %s: %v; none can be used", credentialKeyAttribute, err)
			return nil, nil
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode != http.StatusOK {
			log.Printf("agent credentials: reading %s: %s; none can be used", credentialKeyAttribute, resp.Status)
			return nil, nil
		}
		text = string(b)
	default:
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s: want 32 bytes, base64", credentialKeyAttribute)
	}
	return key, nil
}

// reportGuestAttribute writes value to the VM's guest attribute key, so
// that install, which can't reach the database, reads the result.
func reportGuestAttribute(ctx context.Context, key, value string) {
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodPut,
		metadataHost+"/computeMetadata/v1/instance/guest-attributes/"+key, strings.NewReader(value))
	if err != nil {
		log.Printf("guest attribute %s: %v", key, err)
		return
	}
	req.Header.Set("Metadata-Flavor", "Google")
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		log.Printf("guest attribute %s: %v", key, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("guest attribute %s: %s", key, resp.Status)
	}
}

// schemaCheck says why a store at version v, whose contract steps
// require requires, can't be served by this release, or "".
func schemaCheck(v, requires, latest int) string {
	return store.SchemaCheck(v, requires, migrations.Oldest, latest)
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var sf storeFlags
	sf.register(fs)
	doMigrate := fs.Bool("migrate", false, "migrate the schema before serving (sqlite only, for development)")
	socket := fs.String("socket", "", "serve on this Unix socket")
	listen := fs.String("listen", "", "serve on this TCP address, for tests")
	hostProject := fs.String("host-project", "", "the host project: serve the compute/v1 facade over it")
	fakeGCE := fs.Bool("fake-gce", false, "serve the facade over an in-memory fake GCE (development and tests only)")
	imageProjects := fs.String("image-projects", "", "comma-separated public image projects tenants may read (default the facade's list)")
	facadeEndpoint := fs.String("facade-endpoint", facade.DefaultEndpoint, "the facade's base URL, that selfLinks are written on")
	agentCredentials := fs.String("agent-credentials", "", "comma-separated name=provider pairs naming the deployment's agent credentials (anthropic-api=anthropic), until credentials are stored")
	credentialKeyFile := fs.String("credential-key-file", "", "a file holding silo-credential-key, base64 (development and tests; on a control VM with --agent-sessions silod reads it from the metadata server)")
	sessionDir := fs.String("session-dir", session.DefaultDir, "where each agent session's socket is made, as <dir>/<agent id>/silo.sock")
	sessiondSocket := fs.String("sessiond-socket", "", "run agents' harness sessions through the session launcher on this socket (silod sessiond); with neither this nor --fake-agents, agents stay PROVISIONING")
	sessionStateDir := fs.String("session-state-dir", harness.DefaultLayout.StateDir, "where sessiond keeps each session's private directory, as <dir>/<agent id>")
	sessionHost := fs.String("session-host", "", "this control VM's name, which agents' hosts record (default the hostname)")
	agentSessions := fs.Int("agent-sessions", 0, "install's --agent-sessions: this VM runs as many sessions as it can carry, up to this (0 with --sessiond-socket: as many as it can carry)")
	fakeAgents := fs.Bool("fake-agents", false, "run agents' harness sessions with agents.Fake, which starts nothing and reports each tool server connected (development and tests only)")
	callerHeader := fs.Bool("dev-caller-header", false, "trust a Silo-Caller: <silo id> header as the caller (development and tests only)")
	fenceProject := fs.String("fence-project", "", "apply the fence policy silo-fence in this host project (default: no fence is applied)")
	computeEndpoint := fs.String("compute-endpoint", "", "compute/v1's base URL, for tests against the fake, which also serves the Cloud Quotas read (default GCE's)")
	gatewaySocket := fs.String("gateway-socket", "", "serve the gateway on this Unix socket: the one place a Silo-Origin header is believed")
	gatewayGroup := fs.Int("gateway-socket-group", -1, "give this group id, which the gateway runs in, the gateway socket (mode 0660); default the socket is silod's alone (0600)")
	gatewayReplicas := fs.Bool("gateway-replicas", false, "the gateway runs as a MIG, whose health checks the fence lets in")
	gatewayTLS := fs.String("gateway-tls-listen", "", fmt.Sprintf("serve the gateway, over mTLS, to gateway replicas on this TCP address (install's is :%d); the leader issues their certificates", gwtls.Port))
	gatewayRegistry := fs.String("gateway-registry", "", "with --gateway-tls-listen: serve gateway replicas, which have no token, reads of this image repository (<host>/<path>) with silod's own credentials")
	homeRegion := fs.String("home-region", "", "the home region, where the gateway replicas' MIG is (with --gateway-replicas)")
	frontDoorAddr := fs.String("front-door-listen", "", fmt.Sprintf("serve the front door on this TCP address (install's is :%d): callers with a Google ID token, mapped by the member table", frontdoor.Port))
	frontDoorAudience := fs.String("front-door-audience", "", "the audience a front-door ID token must carry (default silo:<--host-project>)")
	controlReplicas := fs.Int("control-replicas", 1, "how many silod replicas share the store; above 1 needs --store=postgres")
	reconcileMode := fs.String("reconcile", "", "report-only: the reconcile only reports orphans, whatever the database says (default: as the database says)")
	advertise := fs.String("advertise", "", "the address this replica's heartbeat reports (default --listen's)")
	rootMemory := fs.Int64("root-memory", 0, "install's --root-memory, in MiB: the root's memory_mb in each region, which the hourly root capacity refresh sets (0 is unlimited)")
	imageStorage := fs.Int64("image-storage-gb", 0, "the root's image_storage_gb, which the hourly root capacity refresh sets (0 is unlimited)")
	snapshotStorage := fs.Int64("snapshot-storage-gb", 0, "the root's snapshot_storage_gb, which the hourly root capacity refresh sets (0 is unlimited)")
	backendKind := fs.String("backend", "gcp", "the backend: gcp, or local (one machine; needs --data-dir, whose SQLite store is its production store, and silo-nsd)")
	nsdState := fs.String("nsd-state", "", "with --backend=local: the namespace holder's state file (default $XDG_RUNTIME_DIR/silo/nsd.state)")
	nftPath := fs.String("nft", "nft", "with --backend=local: the nft command, run in silo-net through the holder")
	localGateway := fs.String("local-gateway", "10.255.255.1,10.255.255.2", "with --backend=local: silo.internal's and docker.silo.internal's addresses on gw0, comma-separated")
	fs.Parse(args)
	if *socket == "" && *listen == "" && *gatewaySocket == "" && *frontDoorAddr == "" && *gatewayTLS == "" {
		return errors.New("give --socket, --listen, --gateway-socket, --gateway-tls-listen, --front-door-listen, or several")
	}
	if *frontDoorAddr != "" && *frontDoorAudience == "" {
		if *hostProject == "" {
			return errors.New("--front-door-listen needs --front-door-audience, or --host-project to make it from")
		}
		*frontDoorAudience = frontdoor.Audience(*hostProject)
	}
	switch *reconcileMode {
	case "", store.ReconcileReportOnly:
	default:
		return fmt.Errorf("--reconcile=%s: the only mode is report-only", *reconcileMode)
	}
	dd, err := sf.openDataDir(fs)
	if err != nil {
		return err
	}
	switch *backendKind {
	case "gcp":
	case local.Kind:
		// One host is one silod, so SQLite in the data directory is
		// the store (local-backend.md call L22), not Cloud SQL.
		if dd == nil {
			return errors.New("--backend=local needs --data-dir: the local backend's store is the data directory's SQLite database")
		}
		if *hostProject != "" || *fenceProject != "" {
			return errors.New("--backend=local: --host-project and --fence-project are the GCP backend's")
		}
	default:
		return fmt.Errorf("--backend=%s: want gcp or local", *backendKind)
	}
	db, sp, err := openStore(ctx, sf)
	if err != nil {
		return err
	}
	defer db.Close()
	if *controlReplicas > 1 && !db.Shared() {
		return fmt.Errorf("--control-replicas=%d: the %s store is not shared, so it runs one silod, which is always the leader", *controlReplicas, sp.Backend)
	}
	if *doMigrate {
		if sp.Backend != "sqlite" {
			return errors.New("--migrate is refused on postgres: install and upgrade migrate it, with silod migrate")
		}
		if err := db.Migrate(ctx, db.Latest()); err != nil {
			return err
		}
	}
	v, err := db.Schema(ctx)
	if err != nil {
		return err
	}
	requires, err := db.Contracts(ctx)
	if err != nil {
		return err
	}
	why := schemaCheck(v, requires, db.Latest())
	if why != "" {
		log.Printf("schema version %d (contract steps requiring schema %d) is outside what this release runs on (%d to %d, and later expand steps): not serving the API",
			v, requires, migrations.Oldest, db.Latest())
	}
	// repoHost keeps silo repos in the data directory, through its
	// storage driver (local-git.md, "On disk"); without one, silod
	// makes no repos.
	var repoHost fenceapi.RepoHost
	var git *gitserve.Service
	var mir *mirror.Mirror
	if dd != nil && why == "" && v >= store.HeartbeatSchema {
		// The dir driver is the only one; the local backend (step 5 of
		// local-backend.md §15) takes it from here.
		drv := &storage.Dir{Root: dd.Silos()}
		if err := dd.Bind(ctx, db, drv.Name()); err != nil {
			return err
		}
		log.Printf("data directory %s (id %s); storage driver %s: %s", dd.Path, dd.ID, drv.Name(), drv.Guarantees())
		// silod's own hooks, rewritten at every start: re-creatable, so
		// in cache/.
		hooks := filepath.Join(dd.Cache(), "git-hooks")
		if err := gitserve.WriteHooks(hooks); err != nil {
			return fmt.Errorf("writing the git hooks: %w", err)
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		gh := &gitstore.Host{Storage: drv, HooksDir: hooks}
		repoHost = gh
		// No disk_gb class on this backend yet: pushes are not held to
		// one until there is (local-git.md, "Quota").
		git = gitserve.New(gitserve.Options{Host: gh, Hook: exe})
		if v >= store.MirrorSchema {
			// The outbound mirror (local-git.md §8): its credential in
			// keys/mirror/, its quarantine repo in cache/, since it
			// keeps nothing that can't be fetched again.
			mir = mirror.New(mirror.Options{Store: db, Repos: gh,
				Keys: filepath.Join(dd.Keys(), "mirror"), Dir: filepath.Join(dd.Cache(), "mirror")})
		}
	}
	replica := store.Replica{ID: replicaID(), Release: release, Latest: db.Latest(), Started: time.Now(), Address: *advertise}
	if replica.Address == "" {
		replica.Address = *listen
	}
	var sessions atomic.Int64
	var runner *harness.Runner
	if *sessiondSocket != "" && !*fakeAgents {
		host := *sessionHost
		if host == "" {
			host, _ = os.Hostname()
		}
		cpus, memMB := machineSize()
		slots := agents.SessionsPerVM(cpus, memMB)
		if *agentSessions > 0 {
			slots = agents.SessionSlots(*agentSessions, cpus, memMB)
		}
		runner = &harness.Runner{Host: host, Slots: slots, Runtime: &harness.Client{Socket: *sessiondSocket},
			Layout: harness.Layout{SocketDir: *sessionDir, StateDir: *sessionStateDir}}
		replica.Host, replica.AgentSlots = host, slots
		log.Printf("running up to %d harness sessions on %s (%d vCPU, %d MiB), through %s", slots, host, cpus, memMB, *sessiondSocket)
	}
	if why == "" && v >= store.HeartbeatSchema {
		go heartbeat(ctx, db, replica, &sessions, runner)
		if mode, ok, err := store.GetSetting(ctx, db, store.SettingReconcile); err == nil && ok {
			log.Printf("reconcile is %s", mode)
		}
	}
	// leaderLed is whether the leader (SV-52) runs the fence and the
	// reconcile: from the schema with the leader epoch on.
	leaderLed := why == "" && v >= store.LeaderSchema
	var duties []leader.Duty
	cfg := gcefence.DefaultConfig
	var applier *gcefence.Applier
	cfg.GatewayReplicas = *gatewayReplicas
	// nil but in the conformance build (testswitch_conformance.go).
	cfg.KeepControlPort = keepControlPortSwitch()
	go watchTestSwitches(ctx)
	opts := fenceapi.Options{Ready: func() string { return why }, Budget: gcefence.Budget{Config: cfg}, Repos: repoHost}
	if git != nil {
		opts.Git = git
	}
	if mir != nil {
		opts.Mirror = mir
		if leaderLed {
			duties = append(duties, leader.Duty{Name: "mirror", Run: mir.Run})
		} else {
			go mir.Run(ctx)
		}
	}
	if *fenceProject != "" {
		client, err := gceclient.New(ctx, gcpOptions(*computeEndpoint))
		if err != nil {
			return err
		}
		applier = &gcefence.Applier{Store: db, GCE: client, Project: *fenceProject, Config: cfg}
		if leaderLed {
			// Only the leader applies; every replica's fence calls wait
			// on WaitApplied for the leader to record their change.
			applier.Guard = leader.Guard
			applier.OnDrift = reconcile.AuditDrift(db, nil)
		}
	}
	if *callerHeader {
		opts.Caller = HeaderCaller
	}
	opts.FacadeEndpoint = *facadeEndpoint
	var fc *facade.Server
	var hostClient gce.Client
	if *hostProject != "" {
		var client gce.Client
		if *fakeGCE {
			if client, err = devFake(ctx, *hostProject); err != nil {
				return err
			}
		} else if client, err = gceclient.New(ctx, gcpOptions(*computeEndpoint)); err != nil {
			return err
		}
		hostClient = client
		fo := facade.Options{GCE: client, HostProject: *hostProject, Endpoint: *facadeEndpoint, Ready: opts.Ready, Caller: opts.Caller}
		if leaderLed {
			fo.Replica = replica.ID
		}
		if *imageProjects != "" {
			fo.ImageProjects = strings.Split(*imageProjects, ",")
		}
		fc = facade.New(db, fo)
		defer fc.Close()
	}
	bo := gcp.Options{Store: db, Host: hostClient, Project: *hostProject, Fence: applier, Facade: fc}
	// A fake (in process, or on --compute-endpoint) reports no quota it
	// wasn't given, which the refresh would read as no capacity at all.
	if leaderLed && !*fakeGCE && *computeEndpoint == "" {
		bo.Capacity = &rootcap.Config{Project: *hostProject, RootMemoryMB: *rootMemory, ImageStorageGB: *imageStorage,
			SnapshotStorageGB: *snapshotStorage, Guard: leader.Guard}
	}
	var be backend.Backend
	if *backendKind == local.Kind {
		if be, err = localBackend(db, *nsdState, *nftPath, *localGateway, leaderLed); err != nil {
			return err
		}
		if err := bindHolder(ctx, db, *nsdState); err != nil {
			return err
		}
	} else {
		be = gcp.New(bo)
	}
	if be.Fence != nil {
		opts.Fence, opts.Budget = be.Fence, be.Fence
		if leaderLed {
			duties = append(duties, leader.Duty{Name: "fence", Run: func(ctx context.Context) error {
				return be.Fence.Run(ctx, 5*time.Second)
			}})
		} else {
			go runFence(ctx, db, be.Fence)
		}
	}
	opts.Instances = backend.Resolver(be.Targets)
	if be.Classes != nil {
		// quota:preview prices a facade request as the facade would.
		opts.Pricer = be.Classes
	}
	if be.ToolVMs != nil {
		// Agents' tool VMs are made through the facade (SV-104).
		opts.ToolVMs = be.ToolVMs
	}
	if be.Contents != nil {
		opts.Deleter = be.Contents
	}
	if *agentCredentials != "" {
		creds := agents.StaticCredentials{}
		for _, kv := range strings.Split(*agentCredentials, ",") {
			name, provider, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok || name == "" || provider == "" {
				return fmt.Errorf("--agent-credentials %q: want name=provider pairs", *agentCredentials)
			}
			creds[name] = provider
		}
		opts.Credentials = creds
	}
	if opts.CredentialKey, err = credentialKey(ctx, *credentialKeyFile, *agentSessions > 0); err != nil {
		return err
	}
	var fakeRunner *agents.Fake
	// Each session gets its own socket, opened before it starts (agents.md,
	// §5, SV-107).
	sockets := &session.Sockets{Dir: *sessionDir}
	defer sockets.CloseAll()
	if *fakeAgents {
		fakeRunner = &agents.Fake{AutoConnect: true}
		opts.Runner = &session.Runner{Inner: fakeRunner, Sockets: sockets}
	} else if runner != nil {
		// The runner opens a session's socket only on its host.
		runner.Sockets = sockets
		opts.Runner = runner
	}
	api := fenceapi.New(db, opts)
	defer api.Close()
	var gitHandler http.Handler
	if git != nil {
		git.SetAPI(api)
		gitHandler = git
	}
	connected := api.ToolServerConnected
	if runner != nil {
		runner.Control, runner.Logins = api, api
		// A tool server's attach may begin the first turn, which the
		// session's host runs.
		connected = func(ctx context.Context, agent string) error {
			if err := api.ToolServerConnected(ctx, agent); err != nil {
				return err
			}
			return runner.StartTurn(ctx, agent)
		}
	}
	hub := &toolchan.Hub{Connected: connected, Disconnected: api.ToolServerDisconnected}
	sockets.API, sockets.Control, sockets.Tools = api, api, hub
	// The model proxy, for API-key sessions, on their sockets alone
	// (agents.md, "Metering", SV-110).
	sockets.Model = &modelproxy.Proxy{Meter: api}
	if fc != nil {
		sockets.Facade = fc
	}
	if fakeRunner != nil {
		fakeRunner.Connected = api.ToolServerConnected
	}
	if why == "" {
		// They stop with ctx; they are waited for before the store
		// closes, which the defers above do last.
		var expiry sync.WaitGroup
		defer expiry.Wait()
		expiry.Add(2)
		go func() { defer expiry.Done(); runManageExpiry(ctx, db) }()
		go func() { defer expiry.Done(); runAgentExpiry(ctx, db, api) }()
		if opts.Instances != nil {
			expiry.Add(1)
			go func() { defer expiry.Done(); runExportTargets(ctx, db, opts.Instances) }()
		}
	}
	if leaderLed {
		duties = append(duties, leaderDuties(db, leaderConfig{
			backend: be, facade: fc, reportOnly: *reconcileMode == store.ReconcileReportOnly,
		})...)
		if *gatewayReplicas && hostClient != nil && *homeRegion != "" {
			duties = append(duties, gatewayCertDuty(&gwtls.Issuer{Store: db,
				Fleet: &gwtls.GCEFleet{GCE: hostClient, Project: *hostProject, Region: *homeRegion}}))
		}
		if runner != nil && v >= store.HostSchema {
			duties = append(duties, moveSessionsDuty(api))
		}
		go leader.Run(ctx, leader.Options{Store: db, ID: replica.ID, Duties: duties})
	}
	if runner != nil && why == "" && v >= store.HostSchema {
		// Leftover session containers go, then this VM's sessions are
		// resumed (agents.md, "silod restarts", SV-115).
		go func() {
			if err := runner.Run(ctx); err != nil {
				log.Printf("harness sessions: %v", err)
			}
		}()
	}

	var handler http.Handler = api
	if fc != nil {
		if why == "" {
			if err := fc.Resume(ctx); err != nil {
				return err
			}
		}
		mux := http.NewServeMux()
		mux.Handle("/compute/v1/", fc)
		mux.Handle("/", api)
		handler = mux
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ConnState: countSessions(&sessions)}
	errc := make(chan error, 8)
	if *frontDoorAddr != "" {
		v, err := frontdoor.NewGoogle(ctx, *frontDoorAudience)
		if err != nil {
			return err
		}
		fsrv := &http.Server{Handler: frontdoor.Handler(db, frontdoor.Options{Verifier: v, API: handler}), ReadHeaderTimeout: 10 * time.Second}
		defer fsrv.Close()
		l, err := net.Listen("tcp", *frontDoorAddr)
		if err != nil {
			return err
		}
		log.Printf("serving the front door on %s, for ID tokens with audience %s", l.Addr(), *frontDoorAudience)
		go func() { errc <- fsrv.Serve(l) }()
	}
	var gatewayHandler http.Handler
	if *gatewaySocket != "" || *gatewayTLS != "" {
		pub := gateway.NewPublisher(db)
		if why == "" {
			go pub.Run(ctx)
		}
		gatewayHandler = gateway.Silod(db, gateway.SilodOptions{API: handler, Publisher: pub, Tools: &gateway.ToolChannels{
			// Host stays nil: an attach is taken by whichever silod it
			// reaches. Sessions are placed on a control VM now (the
			// agent's host), and redirecting an attach to the session's
			// host needs each control VM's gateway address, which the
			// heartbeat doesn't carry yet.
			Agent: api.ToolChannelAgent,
			Hub:   hub,
		}, ToolVMAgent: api.ToolVMAgentAt, Git: gitHandler})
	}
	if *gatewayTLS != "" {
		// Gateway replicas: only a client whose certificate the leader
		// issued to a gateway VM is believed (package gwtls).
		th := gatewayHandler
		if *gatewayRegistry != "" {
			ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
			if err != nil {
				return err
			}
			reg := &gwtls.Registry{Repository: *gatewayRegistry, Token: func(context.Context) (string, error) {
				t, err := ts.Token()
				if err != nil {
					return "", err
				}
				return t.AccessToken, nil
			}}
			mux := http.NewServeMux()
			mux.Handle("/v2/", reg.Handler())
			mux.Handle("/", gatewayHandler)
			th = mux
		}
		tsrv := &http.Server{Handler: th, ReadHeaderTimeout: 10 * time.Second}
		defer tsrv.Close()
		l, err := tls.Listen("tcp", *gatewayTLS, (&gwtls.Server{Store: db}).Config())
		if err != nil {
			return err
		}
		log.Printf("serving the gateway to gateway replicas, over mTLS, on %s", l.Addr())
		go func() { errc <- tsrv.Serve(l) }()
	}
	if *gatewaySocket != "" {
		gsrv := &http.Server{Handler: gatewayHandler, ReadHeaderTimeout: 10 * time.Second}
		defer gsrv.Close()
		os.Remove(*gatewaySocket)
		l, err := net.Listen("unix", *gatewaySocket)
		if err != nil {
			return err
		}
		// Only the gateway may connect: whoever can is believed about
		// every caller's silo.
		mode := os.FileMode(0o600)
		if *gatewayGroup >= 0 {
			// The gateway runs as its own user, in a group of its own
			// that silod is also in (install's control step).
			if err := os.Chown(*gatewaySocket, -1, *gatewayGroup); err != nil {
				return err
			}
			mode = 0o660
		}
		if err := os.Chmod(*gatewaySocket, mode); err != nil {
			return err
		}
		log.Printf("serving the gateway on %s", *gatewaySocket)
		go func() { errc <- gsrv.Serve(l) }()
	}
	if *socket != "" {
		os.Remove(*socket)
		l, err := net.Listen("unix", *socket)
		if err != nil {
			return err
		}
		log.Printf("serving on %s", *socket)
		go func() { errc <- srv.Serve(l) }()
	}
	if *listen != "" {
		l, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		log.Printf("serving on %s", l.Addr())
		go func() { errc <- srv.Serve(l) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// gcpOptions are the real client's options for --compute-endpoint. A
// compute endpoint other than GCE's is a test's fake, which serves the
// Cloud Quotas read at its root (fake.Server.CloudQuotasEndpoint), so a
// silod pointed at one never calls Google's.
func gcpOptions(computeEndpoint string) gceclient.Options {
	o := gceclient.Options{ComputeEndpoint: computeEndpoint}
	if u, err := url.Parse(computeEndpoint); err == nil && computeEndpoint != "" {
		o.CloudQuotasEndpoint = u.Scheme + "://" + u.Host + "/"
	}
	return o
}

// devFake is --fake-gce's GCE: a fake with the host VPC install would
// have made (the facade's default network, silo), so the facade's subnetworks and instances have a network
// to go in.
func devFake(ctx context.Context, project string) (*fake.Fake, error) {
	f := fake.New(fake.Options{Project: project})
	op, err := f.InsertNetwork(ctx, project, &compute.Network{Name: "silo", NetworkFirewallPolicyEnforcementOrder: "BEFORE_CLASSIC_FIREWALL"})
	if err == nil {
		op, err = f.WaitOperation(ctx, project, gce.Scope{}, op.Name)
	}
	if err == nil && op.Error != nil {
		err = fmt.Errorf("the fake's host VPC: %s", op.Error.Errors[0].Message)
	}
	return f, err
}

// replicaID is this process's replica id: its host and pid.
func replicaID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// heartbeat writes this replica's heartbeat every ten seconds, which
// silod migrate reads before a contract step and the leader reads to
// adopt the operations of a replica that died, and deletes it when ctx
// ends. sessions is the replica's count of open connections.
// localBackend is the local backend over db, with the fence loaded into
// silo-net through the namespace holder whose state file is state.
// There are no runtime drivers yet, so nothing is listed, deleted or
// resolved, and no network is in the fence's topology: tenants' sets
// are complete, and no bridge is let through to the gateway.
func localBackend(db store.Store, state, nft, gateway string, leaderLed bool) (backend.Backend, error) {
	if state == "" {
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if rt == "" {
			return backend.Backend{}, errors.New("--backend=local: give --nsd-state, or set XDG_RUNTIME_DIR")
		}
		state = filepath.Join(rt, "silo", "nsd.state")
	}
	var topo netfence.Topology
	addrs := strings.Split(gateway, ",")
	if len(addrs) != 2 {
		return backend.Backend{}, fmt.Errorf("--local-gateway %q: want silo.internal's and docker.silo.internal's addresses", gateway)
	}
	for i, p := range []*netip.Addr{&topo.API, &topo.Docker} {
		a, err := netip.ParseAddr(strings.TrimSpace(addrs[i]))
		if err != nil || !a.Is4() {
			return backend.Backend{}, fmt.Errorf("--local-gateway %q: %q is not an IPv4 address", gateway, addrs[i])
		}
		*p = a
	}
	o := local.Options{Store: db, NFT: local.HolderNFT(state, nft), Topology: netfence.Static(topo)}
	if leaderLed {
		o.Guard = leader.Guard
		o.OnDrift = reconcile.AuditDriftNames(db, nil)
	}
	return local.New(o), nil
}

// bindHolder waits for silo-nsd, which writes its state once its
// namespaces are ready (fresh ones after a reboot, adopted ones after
// its own restart), and checks that they map what the store recorded
// (local-rootless-storage.md call L33). What isn't on disk is rebuilt
// from the store after it: the fence's applier loads the whole ruleset
// into silo-net, which after a reboot is empty.
func bindHolder(ctx context.Context, db store.Store, state string) error {
	if state == "" {
		state = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "silo", "nsd.state")
	}
	mapping, err := local.HolderMapping(ctx, state, time.Minute)
	if err != nil {
		return fmt.Errorf("--backend=local: %w", err)
	}
	return local.BindMapping(ctx, db, mapping)
}

func heartbeat(ctx context.Context, db store.Store, r store.Replica, sessions *atomic.Int64, runner *harness.Runner) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		r.Sessions = int(sessions.Load())
		if runner != nil {
			r.AgentSessions = runner.Running()
		}
		if err := store.Heartbeat(ctx, db, r); err != nil && ctx.Err() == nil {
			log.Printf("heartbeat: %v", err)
		}
		select {
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			store.Depart(dctx, db, r.ID)
			cancel()
			return
		case <-t.C:
		}
	}
}

// countSessions keeps n at the number of open connections of the
// server whose ConnState it is.
func countSessions(n *atomic.Int64) func(net.Conn, http.ConnState) {
	return func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			n.Add(1)
		case http.StateHijacked, http.StateClosed:
			n.Add(-1)
		}
	}
}

// runGateway runs the gateway (docs/silo/gateway.md). It holds no
// cloud credentials and no store: it reaches silod on silod's gateway
// socket, and keeps serving the tables it has while silod restarts.
// Install places it, and resolves silo.internal to it.
func runGateway(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	silodSocket := fs.String("silod-socket", "", "silod's gateway socket (silod serve --gateway-socket)")
	apiAddr := fs.String("api-listen", fmt.Sprintf(":%d", gateway.APIPort), "the API port's address: /compute/v1/ and /silo/v1/")
	httpAddr := fs.String("http-listen", fmt.Sprintf(":%d", gateway.HTTPPort), "port 80's address: HTTP exports and the agent tool endpoints")
	egressAddr := fs.String("egress-listen", fmt.Sprintf(":%d", gateway.EgressPort), "the egress proxy's address")
	exportHost := fs.String("export-host", "", fmt.Sprintf("the host the TCP export ports (%d-%d) listen on (default all)", gateway.FirstTCPExport, gateway.LastTCPExport))
	toolsDir := fs.String("tools-dir", "", "the directory of the silo-tools binaries port 80 serves to tool VMs ("+gateway.ToolsBinaryName("<arch>")+")")
	silodAddr := fs.String("silod-address", "", "silod's mTLS gateway address, host:port, for a gateway replica (instead of --silod-socket)")
	tlsCert := fs.String("tls-cert", "", "with --silod-address: this gateway's certificate, which the leader issued")
	tlsKey := fs.String("tls-key", "", "with --silod-address: this gateway's key")
	tlsCA := fs.String("tls-ca", "", "with --silod-address: the gateway CA's certificate")
	hostAddr := fs.String("host-address", "", "the address of the control VM this gateway runs on; empty on a gateway-only VM")
	fs.Parse(args)
	var dial func(ctx context.Context) (net.Conn, error)
	silodAt := *silodSocket
	switch {
	case *silodSocket != "" && *silodAddr != "":
		return errors.New("give --silod-socket or --silod-address, not both")
	case *silodSocket != "":
		dial = func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", *silodSocket)
		}
	case *silodAddr != "":
		if *tlsCert == "" || *tlsKey == "" || *tlsCA == "" {
			return errors.New("--silod-address needs --tls-cert, --tls-key and --tls-ca")
		}
		client := &gwtls.Client{CertFile: *tlsCert, KeyFile: *tlsKey, CAFile: *tlsCA}
		dial = func(ctx context.Context) (net.Conn, error) {
			cfg, err := client.Config()
			if err != nil {
				return nil, err
			}
			d := &tls.Dialer{Config: cfg}
			return d.DialContext(ctx, "tcp", *silodAddr)
		}
		silodAt = *silodAddr + " (mTLS)"
	default:
		return errors.New("give --silod-socket or --silod-address")
	}
	var host netip.Addr
	if *hostAddr != "" {
		a, err := netip.ParseAddr(*hostAddr)
		if err != nil {
			return fmt.Errorf("--host-address: %w", err)
		}
		host = a
	}
	g := gateway.New(gateway.Options{Dial: dial, TCPHost: *exportHost, ToolsDir: *toolsDir, HostAddress: host,
		SkipControlNow: skipControlGateSwitch()})
	api, err := net.Listen("tcp", *apiAddr)
	if err != nil {
		return err
	}
	web, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		api.Close()
		return err
	}
	out, err := net.Listen("tcp", *egressAddr)
	if err != nil {
		api.Close()
		web.Close()
		return err
	}
	log.Printf("gateway: API port on %s, port 80 on %s, egress proxy on %s, silod at %s", api.Addr(), web.Addr(), out.Addr(), silodAt)
	return g.Serve(ctx, api, web, out)
}

// runFence applies the fence while this process holds the "fence"
// lease, so that one replica applies it at a time.
func runFence(ctx context.Context, db store.Store, a backend.Fence) {
	for ctx.Err() == nil {
		lost, err := db.Lead(ctx, "fence")
		if err != nil {
			return
		}
		lctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-lost:
			case <-lctx.Done():
			}
			cancel()
		}()
		log.Printf("applying the fence policy")
		if err := a.Run(lctx, 5*time.Second); err != nil && ctx.Err() == nil {
			log.Printf("fence: %v", err)
		}
		cancel()
	}
}

// runManageExpiry ends manage grants whose lease has run out, while
// this process holds the "manage-expiry" lease. A grant stops working
// the moment it runs out whatever this does; this is what audits it and
// tells the gateway.
func runManageExpiry(ctx context.Context, db store.Store) {
	for ctx.Err() == nil {
		lost, err := db.Lead(ctx, "manage-expiry")
		if err != nil {
			return
		}
		t := time.NewTicker(10 * time.Second)
	lead:
		for {
			select {
			case <-ctx.Done():
				break lead
			case <-lost:
				break lead
			case <-t.C:
				if n, err := fenceapi.ExpireManageGrants(ctx, db); err != nil && ctx.Err() == nil {
					log.Printf("manage grants: %v", err)
				} else if n > 0 {
					log.Printf("manage grants: %d expired", n)
				}
			}
		}
		t.Stop()
	}
}

// runExportTargets follows the instances that exports name as their
// targets, while this process holds the "export-targets" lease: an
// export whose instance's address changed is moved to the new one, and
// the fence and the gateway follow.
func runExportTargets(ctx context.Context, db store.Store, resolve fenceapi.InstanceResolver) {
	for ctx.Err() == nil {
		lost, err := db.Lead(ctx, "export-targets")
		if err != nil {
			return
		}
		t := time.NewTicker(30 * time.Second)
	lead:
		for {
			select {
			case <-ctx.Done():
				break lead
			case <-lost:
				break lead
			case <-t.C:
				if n, err := fenceapi.FollowInstanceTargets(ctx, db, resolve); err != nil && ctx.Err() == nil {
					log.Printf("export targets: %v", err)
				} else if n > 0 {
					log.Printf("export targets: %d moved", n)
				}
			}
		}
		t.Stop()
	}
}

// runAgentExpiry warns agents whose lease ends within ten minutes,
// deletes those whose lease has run out, and starts agents' new budget
// periods, while this replica holds the agent-expiry lease (agents.md,
// "The lease runs out" and "Budget runs out").
func runAgentExpiry(ctx context.Context, db store.Store, api *fenceapi.Server) {
	for ctx.Err() == nil {
		lost, err := db.Lead(ctx, "agent-expiry")
		if err != nil {
			return
		}
		t := time.NewTicker(10 * time.Second)
	lead:
		for {
			select {
			case <-ctx.Done():
				break lead
			case <-lost:
				break lead
			case <-t.C:
				if n, err := api.WarnLeases(ctx); err != nil && ctx.Err() == nil {
					log.Printf("agents: lease warnings: %v", err)
				} else if n > 0 {
					log.Printf("agents: %d warned their lease ends soon", n)
				}
				if n, err := api.ExpireAgents(ctx); err != nil && ctx.Err() == nil {
					log.Printf("agents: %v", err)
				} else if n > 0 {
					log.Printf("agents: %d expired", n)
				}
				if n, err := api.ResetBudgetPeriods(ctx); err != nil && ctx.Err() == nil {
					log.Printf("agents: budget periods: %v", err)
				} else if n > 0 {
					log.Printf("agents: %d started a new budget period", n)
				}
			}
		}
		t.Stop()
	}
}

// HeaderCaller takes the caller from a Silo-Caller header, for
// development and tests until the gateway, the front door and the
// session sockets say who is calling.
func HeaderCaller(r *http.Request) (fenceapi.Caller, error) {
	id := strings.TrimSpace(r.Header.Get("Silo-Caller"))
	if id == "" {
		return fenceapi.Caller{}, errors.New("no Silo-Caller header")
	}
	return fenceapi.Caller{Silo: id, Via: fenceapi.ViaInternal}, nil
}
