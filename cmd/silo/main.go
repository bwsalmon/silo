// Command silo runs the Silo hub (docs/hub.md):
//
//	silo hub serve     run the hub: the rendezvous nodes connect to, the directory, the backlog and its dispatcher, live events
//	silo hub migrate   create the hub's tables as the schema's owner, and grant the role the hub serves as its rows
//	silo hub team      create a team, or add admins and repositories to one, straight in the database
//	silo hub token     mint a node token, straight in the database
//
// The last two are for bootstrapping a hub that has no hub admin who
// can reach its API yet (docs/deploy.md): they act on the database the
// hub uses, as whoever can reach it, with no sign-in. Every flag can
// also be set from the environment variable its help names.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/hub/claims"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/server"
	"github.com/bwsalmon/silo/hub/tasks"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "silo:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  silo hub serve [flags]   run the hub
  silo hub migrate [flags] create the hub's tables, and grant the hub's runtime role its rows
  silo hub team [flags]    create a team, or add admins and repositories to one
  silo hub token [flags]   mint a node token
  silo conformance [flags] [-- command...]
                           check that a Grain speaks the task interface (docs/conformance.md)
  silo conformance grain [flags]
                           run the reference Grain, which passes it
Run a command with -h for its flags.`

func run(args []string) error {
	if len(args) >= 1 && args[0] == "conformance" {
		if len(args) >= 2 && args[1] == "grain" {
			return conformanceGrain(args[2:])
		}
		return conformanceKit(args[1:])
	}
	if len(args) < 2 || args[0] != "hub" {
		fmt.Fprintln(os.Stderr, usage)
		return flag.ErrHelp
	}
	switch args[1] {
	case "serve":
		return serve(args[2:])
	case "migrate":
		return migrate(args[2:])
	case "team":
		return team(args[2:])
	case "token":
		return token(args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		return flag.ErrHelp
	}
}

// env is the environment's value for name, or def.
func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// envBool is the environment's boolean value for name, false when it
// is unset or not a boolean.
func envBool(name string) bool {
	b, _ := strconv.ParseBool(os.Getenv(name))
	return b
}

// list splits a comma-separated list, dropping empty entries.
func list(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// multi is a repeatable flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func databaseFlag(fs *flag.FlagSet) *string {
	return fs.String("database", env("SILO_HUB_DATABASE", ""),
		"the hub's database: postgres://... (sqlite:PATH for local development only) [SILO_HUB_DATABASE]")
}

// migrateFlag is whether a command creates the hub's tables that are
// missing, as its own database role: true for development, false where
// `silo hub migrate` made them as the schema's owner and the hub serves
// as a role that cannot (docs/deploy.md).
func migrateFlag(fs *flag.FlagSet) *bool {
	def := true
	if v, ok := os.LookupEnv("SILO_HUB_MIGRATE"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			def = b
		} else {
			fmt.Fprintf(os.Stderr, "silo: SILO_HUB_MIGRATE=%q is not a boolean; creating tables\n", v)
		}
	}
	return fs.Bool("migrate", def,
		"create the hub's tables if they are missing, as -database's role; false when `silo hub migrate` made them and -database's role may only read and write rows [SILO_HUB_MIGRATE]")
}

// openDirectory opens the directory for team and token, creating its
// tables only if create.
func openDirectory(ctx context.Context, url string, create bool) (*directory.Store, error) {
	if create {
		return directory.Open(ctx, url)
	}
	return directory.OpenExisting(ctx, url)
}

func migrate(args []string) error {
	fs := flag.NewFlagSet("silo hub migrate", flag.ContinueOnError)
	db := fs.String("database", env("SILO_HUB_MIGRATE_DATABASE", env("SILO_HUB_DATABASE", "")),
		"the hub's database, as the role that owns its schema: postgres://... (sqlite:PATH for local development only) [SILO_HUB_MIGRATE_DATABASE, else SILO_HUB_DATABASE]")
	role := fs.String("runtime-role", env("SILO_HUB_RUNTIME_ROLE", ""),
		"the PostgreSQL role `silo hub serve -migrate=false` connects as: granted read and write on the hub's tables, now and as later migrations add them, and refused if it could change the schema; empty grants nothing [SILO_HUB_RUNTIME_ROLE]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return errors.New("silo hub migrate: -database is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := server.Migrate(ctx, *db, *role); err != nil {
		return err
	}
	if *role != "" {
		log.Printf("hub: tables migrated; %s may read and write them", *role)
	} else {
		log.Printf("hub: tables migrated")
	}
	return nil
}

func envInt64(name string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	return v
}

func buildServeConfig(args []string) (string, server.Config, error) {
	fs := flag.NewFlagSet("silo hub serve", flag.ContinueOnError)
	addr := fs.String("addr", env("SILO_HUB_ADDR", ":8080"), "address to listen on [SILO_HUB_ADDR]")
	db := databaseFlag(fs)
	doMigrate := migrateFlag(fs)
	iap := fs.String("iap-audience", env("SILO_HUB_IAP_AUDIENCE", ""),
		"sign humans in behind IAP: the backend service's audience, /projects/NUMBER/global/backendServices/ID [SILO_HUB_IAP_AUDIENCE]")
	issuer := fs.String("oidc-issuer", env("SILO_HUB_OIDC_ISSUER", ""), "sign humans in with OIDC: the provider's issuer URL [SILO_HUB_OIDC_ISSUER]")
	clientID := fs.String("oidc-client-id", env("SILO_HUB_OIDC_CLIENT_ID", ""), "the OIDC client ID [SILO_HUB_OIDC_CLIENT_ID]")
	redirect := fs.String("oidc-redirect-url", env("SILO_HUB_OIDC_REDIRECT_URL", ""), "the hub's OIDC callback URL [SILO_HUB_OIDC_REDIRECT_URL]")
	dev := fs.Bool("dev", envBool("SILO_HUB_DEV"),
		"local development and tests only: allow a sqlite: -database, which is refused otherwise and always on Kubernetes; -dev-user-header implies it [SILO_HUB_DEV]")
	devHeader := fs.String("dev-user-header", env("SILO_HUB_DEV_USER_HEADER", ""),
		"INSECURE, local development only: take the caller's email from this header, unverified [SILO_HUB_DEV_USER_HEADER]")
	admins := fs.String("admins", env("SILO_HUB_ADMINS", ""), "comma-separated hub admins, user:EMAIL or group:NAME [SILO_HUB_ADMINS]")
	metricsAddr := fs.String("metrics-addr", env("SILO_HUB_METRICS_ADDR", ""),
		"address to serve Prometheus metrics on, at /metrics; keep it off the load balancer's routes [SILO_HUB_METRICS_ADDR]")
	nodeUI := fs.String("node-ui-domain", env("SILO_HUB_NODE_UI_DOMAIN", ""),
		"serve each node's own UI, to its owner, at <node>.<this domain> (docs/hub.md §11); empty serves none [SILO_HUB_NODE_UI_DOMAIN]")
	grace := fs.Duration("shutdown-grace", server.DefaultShutdownGrace, "how long shutdown waits for nodes and requests")
	heartbeat := fs.Duration("heartbeat", rendezvous.DefaultHeartbeat, "the interval nodes heartbeat at; a node is stale after three missed")
	retry := fs.Duration("dispatch-retry", tasks.DefaultRetry, "how often the dispatcher looks at the queue unprompted")
	pod := fs.String("pod", env("SILO_HUB_POD", ""),
		"this pod's name in the shared node registry (docs/hub.md §2.2.2); the host's name when empty [SILO_HUB_POD]")
	podAddr := fs.String("pod-addr", env("SILO_HUB_POD_ADDR", ""),
		"the address the hub's other pods reach this one at; the host's name when empty [SILO_HUB_POD_ADDR]")
	internalAddr := fs.String("internal-addr", env("SILO_HUB_INTERNAL_ADDR", ""),
		"address to serve the internal listener on, which only the hub's other pods may reach (docs/hub.md §2.2.3); empty serves none [SILO_HUB_INTERNAL_ADDR]")
	internalKey := fs.String("internal-key-file", env("SILO_HUB_INTERNAL_KEY_FILE", ""),
		"the file holding the key the hub's pods share for their internal listeners, at least 32 bytes [SILO_HUB_INTERNAL_KEY_FILE]")
	claimsIssuer := fs.String("claims-issuer", env("SILO_HUB_CLAIMS_ISSUER", ""),
		"sign the claims each task offer carries as this issuer, the silo_issuer of nodes' policies: the hub's URL (docs/hub.md §6.4) [SILO_HUB_CLAIMS_ISSUER]")
	claimsKeys := fs.String("claims-key-file", env("SILO_HUB_CLAIMS_KEY_FILE", ""),
		"comma-separated files holding the PEM private keys (EC P-256 or RSA) task claims are signed with: the first signs, all are published at "+claims.KeysPath+" [SILO_HUB_CLAIMS_KEY_FILE]")
	webhookSecret := fs.String("webhook-secret", env("SILO_HUB_WEBHOOK_SECRET", env("SILO_HUB_GITHUB_WEBHOOK_SECRET", "")),
		"shared HMAC secret verifying X-Hub-Signature-256 on /v1/webhooks/github [SILO_HUB_WEBHOOK_SECRET]")
	webhookSecretFile := fs.String("webhook-secret-file", env("SILO_HUB_WEBHOOK_SECRET_FILE", env("SILO_HUB_GITHUB_WEBHOOK_SECRET_FILE", "")),
		"file holding the shared HMAC secret for /v1/webhooks/github [SILO_HUB_WEBHOOK_SECRET_FILE]")
	githubToken := fs.String("github-token", env("SILO_HUB_GITHUB_TOKEN", ""),
		"GitHub API token used by the Hub to fetch Grainfiles and authorized references [SILO_HUB_GITHUB_TOKEN]")
	githubTokenFile := fs.String("github-token-file", env("SILO_HUB_GITHUB_TOKEN_FILE", ""),
		"file holding the GitHub API token used by the Hub [SILO_HUB_GITHUB_TOKEN_FILE]")
	githubAPIURL := fs.String("github-api-url", env("SILO_HUB_GITHUB_API_URL", ""),
		"base URL of the GitHub REST API (defaults to https://api.github.com) [SILO_HUB_GITHUB_API_URL]")
	githubAppID := fs.Int64("github-app-id", envInt64("SILO_HUB_GITHUB_APP_ID"),
		"GitHub App ID for minting installation tokens when fetching Grainfiles [SILO_HUB_GITHUB_APP_ID]")
	githubAppKeyFile := fs.String("github-app-key-file", env("SILO_HUB_GITHUB_APP_KEY_FILE", env("SILO_HUB_GITHUB_APP_PRIVATE_KEY_FILE", "")),
		"file holding the PEM private key for the GitHub App [SILO_HUB_GITHUB_APP_KEY_FILE]")
	if err := fs.Parse(args); err != nil {
		return "", server.Config{}, err
	}
	if *db == "" {
		return "", server.Config{}, errors.New("silo hub serve: -database is required: postgres://... (sqlite:PATH with -dev, for local development)")
	}
	if err := server.CheckDevSignOn(*devHeader, os.Getenv); err != nil {
		return "", server.Config{}, err
	}
	if err := server.CheckDatabase(*db, *dev || *devHeader != "", os.Getenv); err != nil {
		return "", server.Config{}, err
	}
	var pems [][]byte
	for _, f := range list(*claimsKeys) {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", server.Config{}, fmt.Errorf("a claims key: %w", err)
		}
		pems = append(pems, b)
	}
	var key []byte
	if *internalKey != "" {
		b, err := os.ReadFile(*internalKey)
		if err != nil {
			return "", server.Config{}, fmt.Errorf("the internal key: %w", err)
		}
		key = bytes.TrimSpace(b)
	}
	whSecret := strings.TrimSpace(*webhookSecret)
	if *webhookSecretFile != "" {
		b, err := os.ReadFile(*webhookSecretFile)
		if err != nil {
			return "", server.Config{}, fmt.Errorf("the webhook secret: %w", err)
		}
		whSecret = strings.TrimSpace(string(b))
	}
	ghToken := strings.TrimSpace(*githubToken)
	if *githubTokenFile != "" {
		b, err := os.ReadFile(*githubTokenFile)
		if err != nil {
			return "", server.Config{}, fmt.Errorf("the github token: %w", err)
		}
		ghToken = strings.TrimSpace(string(b))
	}
	var ghAppKey []byte
	if rawKey := os.Getenv("SILO_HUB_GITHUB_APP_PRIVATE_KEY"); rawKey != "" {
		ghAppKey = []byte(rawKey)
	}
	if *githubAppKeyFile != "" {
		b, err := os.ReadFile(*githubAppKeyFile)
		if err != nil {
			return "", server.Config{}, fmt.Errorf("the github app private key: %w", err)
		}
		ghAppKey = b
	}
	cfg := server.Config{
		Database:     *db,
		NoMigrate:    !*doMigrate,
		IAPAudience:  *iap,
		OIDCIssuer:   *issuer,
		OIDCClientID: *clientID,
		// Secrets come only from the environment, never a flag, so
		// they do not show in the process list.
		OIDCClientSecret:    os.Getenv("SILO_HUB_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:     *redirect,
		OIDCSessionKey:      []byte(os.Getenv("SILO_HUB_OIDC_SESSION_KEY")),
		DevUserHeader:       *devHeader,
		HubAdmins:           list(*admins),
		NodeUIDomain:        *nodeUI,
		ShutdownGrace:       *grace,
		Heartbeat:           *heartbeat,
		DispatchRetry:       *retry,
		MetricsAddr:         *metricsAddr,
		Pod:                 *pod,
		PodAddr:             *podAddr,
		InternalAddr:        *internalAddr,
		InternalKey:         key,
		ClaimsIssuer:        *claimsIssuer,
		ClaimsKeys:          pems,
		WebhookSecret:       whSecret,
		GitHubToken:         ghToken,
		GitHubAPIURL:        strings.TrimSpace(*githubAPIURL),
		GitHubAppID:         *githubAppID,
		GitHubAppPrivateKey: ghAppKey,
		Logf:                log.Printf,
	}
	return *addr, cfg, nil
}

func serve(args []string) error {
	addr, cfg, err := buildServeConfig(args)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// SIGTERM is how Kubernetes stops a pod; the hub says goodbye to
	// its nodes so that they reconnect to its replacement.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return server.Serve(ctx, l, cfg)
}

func team(args []string) error {
	fs := flag.NewFlagSet("silo hub team", flag.ContinueOnError)
	db := databaseFlag(fs)
	doMigrate := migrateFlag(fs)
	id := fs.String("id", "", "the team's ID, lower-case: created if it does not exist")
	name := fs.String("name", "", "the team's display name, when it is created")
	var admins, members, repos multi
	fs.Var(&admins, "admin", "an admin of the team, user:EMAIL or group:NAME (repeatable)")
	fs.Var(&members, "member", "a member of the team, user:EMAIL or group:NAME (repeatable)")
	fs.Var(&repos, "repo", "a repository, owner/name, the team owns (repeatable)")
	lostRequeue := fs.String("lost-requeue-after", "",
		"requeue the team's lost running tasks after this long, at least 5m; 0 turns it off (docs/hub.md §6.3)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" || *id == "" {
		return errors.New("silo hub team: -database and -id are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := openDirectory(ctx, *db, *doMigrate)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.CreateTeam(ctx, *id, *name); err != nil && !errors.Is(err, directory.ErrExists) {
		return err
	}
	for _, p := range admins {
		if err := s.SetMember(ctx, *id, p, directory.Admin); err != nil {
			return err
		}
	}
	for _, p := range members {
		if err := s.SetMember(ctx, *id, p, directory.Member); err != nil {
			return err
		}
	}
	for _, r := range repos {
		if err := s.AssignRepo(ctx, *id, r); err != nil {
			return err
		}
	}
	if *lostRequeue != "" {
		d, err := time.ParseDuration(*lostRequeue)
		if err != nil {
			return fmt.Errorf("silo hub team: -lost-requeue-after: %w", err)
		}
		if err := s.SetTeamSettings(ctx, *id, directory.Settings{LostRequeueAfter: d}); err != nil {
			return err
		}
		fmt.Printf("team %s: lost running tasks requeued after %s (0: never)\n", *id, d)
	}
	fmt.Printf("team %s: %d admins, %d members and %d repositories added\n", *id, len(admins), len(members), len(repos))
	return nil
}

func token(args []string) error {
	fs := flag.NewFlagSet("silo hub token", flag.ContinueOnError)
	db := databaseFlag(fs)
	doMigrate := migrateFlag(fs)
	owner := fs.String("owner", "", "the node's owner, the email its tasks are admitted under")
	node := fs.String("node", "", "the node's ID: the only ID the token may register")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" || *owner == "" || *node == "" {
		return errors.New("silo hub token: -database, -owner and -node are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := openDirectory(ctx, *db, *doMigrate)
	if err != nil {
		return err
	}
	defer s.Close()
	tok, rec, err := s.MintNodeToken(ctx, *owner, *node)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "minted token %s for node %s of %s; it is shown only once:\n", rec.ID, rec.Node, rec.Owner)
	fmt.Println(tok)
	return nil
}
