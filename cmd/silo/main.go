// Command silo is the command-line client of Silo's fence API
// (docs/silo/README.md, "silo, the CLI"; docs/silo/fence-api.md).
//
//	silo describe [silo] [--subtree]
//	silo children [silo]
//	silo create <path> --address /20|<cidr> [--regions ...] [--quota ...] ...
//	silo patch <silo> [the create flags]
//	silo delete <silo> [--force]
//	silo grow <silo> --to /19 [--block <cidr>] [--add-block]
//	silo block add <silo> --address /22|<cidr>
//	silo block release <silo> <cidr>
//	silo quota preview <silo> --quota ...
//	silo members get|put|delete <silo> [identity]
//	silo audit [silo] [--since ...] [--kind ...] [--subtree]
//	silo agent create|list|get|patch|delete|stop|resume|renew ...
//	silo credential add|login|list|remove ...
//	silo model-price list|set ...
//	silo install --project ... --regions ... --admin ... [flags]
//	silo uninstall --project ... [--force] [--delete-data] [--dry-run]
//	silo proxy [--listen localhost:8642] [--project ...]
//
// Inside the cloud it calls http://silo.internal:8080/silo/v1/ with no
// credentials; --endpoint or $SILO_ENDPOINT points it elsewhere.
// Outside the cloud (--front-door, or when silo.internal doesn't resolve
// and --project or $SILO_PROJECT names the host project) it goes
// through the front door: an IAP tunnel to the control VM, with a
// Google ID token (docs/silo/gateway.md, SV-88).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
	"github.com/bwsalmon/silo/pkg/client"
)

const usage = `usage: silo <command> [flags]

commands:
  describe [silo] [--subtree]       show a silo (default: the caller's own)
  children [silo]                   list a silo's direct children
  create <path> --address /n|cidr   make a silo under its parent
  patch <silo>                      change a silo's allocations and policies
  delete <silo> [--force]           delete a silo, and with --force its subtree
  grow <silo> --to /n               widen a block, or with --add-block add one
  block add <silo> --address /n|cidr
  block release <silo> <cidr>
  quota preview <silo> --quota ...  what an allocation change would cost
  members get <silo> [identity]     list a silo's members, or read one
  members put <silo> <identity>     map an identity to a silo
  members delete <silo> <identity>  unmap it
  export offer <name> --target ip:port --to /silo,... [--protocol http|tcp]
                                    offer an export through the gateway
  export list                       list the caller's exports and who has them
  export withdraw <name> [--to /silo]
  import accept <exporter path>/<name>
                                    accept an export offered to the caller
  import list                       list the caller's imports and offers
  import delete <id>                give up an import
  link propose <name> --peer /silo --direction outbound|inbound|both --rules tcp:5432,...
                                    propose an L3 link; ACTIVE once the peer
                                    proposes the mirror image
  link list                         list the caller's links
  link delete <name>                end a link, for both sides
  repo create [silo/]<name> [--default-branch main] [--seed /team-a/bar]
                                    make a git repo, empty or a copy of one
                                    the caller may read
  repo list [--recursive] [silo]    list a silo's repos
  repo get|delete [silo/]<name>     read or delete a repo
  repo url [silo/]<name> [--by-id]  the URL the caller clones it by
  repo clone [silo/]<name> [dir]    on the host: git clone silo::<path>,
                                    through git-remote-silo (silo, linked
                                    under that name on $PATH)
  repo archives [silo/]<name>       its archive refs: tips a force-push or
                                    a deletion would have orphaned
  repo restore-ref [silo/]<name> <archive> <branch>
                                    make a branch from an archive ref
  registry list [--recursive] [silo]
                                    list a silo's image repos
  registry get|tags [silo/]<name>   read an image repo, or its tags and manifests
  registry url [silo/]<name> [--by-id]
                                    the name the caller pushes to and pulls from
  registry delete [silo/]<name>[:tag|@digest]
                                    delete an image repo, a tag, or a manifest
  registry prune [silo/]<name> --untagged
                                    delete the manifests no tag points at
  audit [silo] [--since --kind --subtree]
  agent create <silo>/agents/<name> --credential --model --budget --tool-zone --tool-subnet
                                    make an agent and its tool VM
  agent list [silo]                 list a silo's agents
  agent get|delete|stop|resume <agent>
  agent patch <agent> [--budget] [--labels]
  agent renew <agent> --ttl|--expire-time
  credential add <name> --provider p --kind api_key < key
                                    add an agent credential (root only)
  credential login <name> --provider p [--from <home>]
                                    log in a subscription and store its files
  credential list|remove <name>     list or remove them; never read back
  model-price list                  the model price table
  model-price set <provider>/<model> --input-usd --output-usd --cache-write-usd --cache-read-usd
                                    override a price (root only; --delete removes it)
  reconcile [--apply]               show the reconcile's mode, or leave report-only
                                    after a restore (root only)
  mirror set --url u --credential-file f [--prefix silo] [--index-branch main | --no-index]
                                    mirror every repo into an uber repo (root only)
  mirror status [--all]             the repos that aren't in sync, and why
  mirror retry [repo]               run a repo's pass now, or retry every repo
  mirror adopt <repo> [--ref r] [--keep-external]
                                    resolve refs the uber repo diverged on
  install --project p --regions r,... --admin user:...
                                    set Silo up in a project (docs/silo/install.md)
  install --backend local [--system] set the local backend up on this machine: user
                                    units as you, or system units under a silo account
  uninstall --project p             remove what install set up
  proxy [--listen localhost:8642]   serve the facade and fence API on localhost,
                                    through the front door, for gcloud and terraform
  unshare [--probe] [-- cmd...]     run cmd as root in the local backend's namespace
                                    holder, or check this host's prerequisites

A silo is named by its path (/team-a/ci), by its id (k7f3qa), or as
self. An agent is named <silo>/agents/<name> (/team-a/agents/fixer), or by
its id. Every command but install and uninstall takes --endpoint
(default $SILO_ENDPOINT, or ` + client.DefaultEndpoint + `),
--output table|json and --timeout, and goes through the front door
outside the cloud: --front-door, --project (default $SILO_PROJECT),
--zone and --instance.
A repo or image repo is named [silo/]name, its silo a path relative to the
caller's own unless it starts with /.
Every change takes --request-id and --validate-only. The export,
import and link commands take --silo, to act for a descendant (or a silo a
manage grant with the grants scope covers).
Run silo <command> -h for a command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &cli{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == RemoteHelper {
		// git runs silo by this name for silo:: URLs.
		args = append([]string{RemoteHelper}, args...)
	}
	os.Exit(c.run(ctx, args))
}

// cli is one invocation of silo, with what tests replace.
type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	// transport carries the API calls: nil is the default transport.
	// The front door's transport comes in here.
	transport http.RoundTripper
	// proxyAddr is the egress proxy to probe when the API port times
	// out: "" is the endpoint's host on port 3128.
	proxyAddr string
	// poll is how often delete polls its operation.
	poll time.Duration
	// The front door's seams: nil is the real one each time.
	tunnel  tunneler
	mint    minter
	resolve func(ctx context.Context, host string) error
	// proxyReady is told silo proxy's address once it serves.
	proxyReady func(addr string)

	// Set by the common flags of the command being run.
	endpoint string
	output   string
	timeout  time.Duration
	front    frontDoorFlags
	api      *client.Client
	// door is the front door a command went through, closed when it
	// ends.
	door *frontDoor
}

// usageError is a mistake on the command line: exit 2, with usage.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// command is one of silo's commands.
type command func(c *cli, ctx context.Context, args []string) error

var commands = map[string]command{
	"describe": (*cli).describe,
	"children": (*cli).children,
	"create":   (*cli).create,
	"patch":    (*cli).patch,
	"delete":   (*cli).delete,
	"grow":     (*cli).grow,
	"block":    group("block", map[string]command{"add": (*cli).blockAdd, "release": (*cli).blockRelease}),
	"quota":    group("quota", map[string]command{"preview": (*cli).quotaPreview}),
	"members": group("members", map[string]command{
		"get": (*cli).membersGet, "list": (*cli).membersGet, "put": (*cli).membersPut, "delete": (*cli).membersDelete,
	}),
	"export": group("export", map[string]command{
		"offer": (*cli).exportOffer, "list": (*cli).exportList, "withdraw": (*cli).exportWithdraw,
	}),
	"import": group("import", map[string]command{
		"accept": (*cli).importAccept, "list": (*cli).importList, "delete": (*cli).importDelete,
	}),
	"link": group("link", map[string]command{
		"propose": (*cli).linkPropose, "list": (*cli).linkList, "delete": (*cli).linkDelete,
	}),
	"repo": group("repo", map[string]command{
		"create": (*cli).repoCreate, "list": (*cli).repoList, "get": (*cli).repoGet, "delete": (*cli).repoDelete, "url": (*cli).repoURL,
		"clone": (*cli).repoClone, "archives": (*cli).repoArchives, "restore-ref": (*cli).repoRestoreRef,
	}),
	RemoteHelper: (*cli).gitRemote,
	"registry": group("registry", map[string]command{
		"list": (*cli).registryList, "get": (*cli).registryGet, "tags": (*cli).registryTags, "url": (*cli).registryURL,
		"delete": (*cli).registryDelete, "prune": (*cli).registryPrune,
	}),
	"audit": (*cli).audit,
	"agent": group("agent", map[string]command{
		"create": (*cli).agentCreate, "list": (*cli).agentList, "get": (*cli).agentGet, "patch": (*cli).agentPatch,
		"delete": (*cli).agentDelete, "stop": agentAction("stop"), "resume": agentAction("resume"), "renew": (*cli).agentRenew,
	}),
	"credential": group("credential", map[string]command{"add": (*cli).credentialAdd, "login": (*cli).credentialLogin,
		"list": (*cli).credentialList, "remove": (*cli).credentialRemove}),
	"model-price": group("model-price", map[string]command{"list": (*cli).modelPriceList, "set": (*cli).modelPriceSet}),
	"reconcile":   (*cli).reconcile,
	"mirror": group("mirror", map[string]command{
		"set": (*cli).mirrorSet, "status": (*cli).mirrorStatus, "retry": (*cli).mirrorRetry, "adopt": (*cli).mirrorAdopt,
	}),
	"install":   (*cli).install,
	"uninstall": (*cli).uninstall,
	"proxy":     (*cli).proxy,
	"unshare":   (*cli).unshare,
}

// group is a command with subcommands, as "block add".
func group(name string, subs map[string]command) command {
	return func(c *cli, ctx context.Context, args []string) error {
		if len(args) == 0 || subs[args[0]] == nil {
			var names []string
			for n := range subs {
				names = append(names, n)
			}
			sort.Strings(names)
			return usagef("usage: silo %s %s ...", name, strings.Join(names, "|"))
		}
		return subs[args[0]](c, ctx, args[1:])
	}
}

// run runs one command line and returns the exit status: 0, 1 for a
// failed call, 2 for a bad command line.
func (c *cli) run(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(c.stderr, usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	cmd := commands[args[0]]
	if cmd == nil {
		fmt.Fprintf(c.stderr, "silo: no command %q\n\n%s", args[0], usage)
		return 2
	}
	err := cmd(c, ctx, args[1:])
	if c.door != nil {
		c.door.Close()
	}
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(c.stderr, "silo %s: %s\n", args[0], ue.msg)
		return 2
	}
	c.renderError(ctx, err)
	return 1
}

// flags is a command's flag set, with the flags every command takes.
func (c *cli) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("silo "+name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	ep := c.getenv("SILO_ENDPOINT")
	if ep == "" {
		ep = client.DefaultEndpoint
	}
	fs.StringVar(&c.endpoint, "endpoint", ep, "the fence API's base URL (default $SILO_ENDPOINT, or the in-cloud endpoint)")
	fs.StringVar(&c.output, "output", "table", "table or json")
	fs.StringVar(&c.output, "o", "table", "short for --output")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Second, "how long each API call may take")
	c.frontDoorFlags(fs, &c.front, true)
	return fs
}

// mutation adds --request-id and --validate-only to fs.
func mutation(fs *flag.FlagSet) *client.Mutation {
	m := &client.Mutation{}
	fs.StringVar(&m.RequestID, "request-id", "", "an idempotency key (a UUID): a repeat within 24 hours returns the first call's result")
	fs.BoolVar(&m.ValidateOnly, "validate-only", false, "run every check and change nothing")
	return m
}

// parse parses args with flags and positional arguments in any order,
// and checks that there are between min and max positional ones.
func (c *cli) parse(fs *flag.FlagSet, args []string, min, max int, what string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if len(pos) < min || len(pos) > max {
		return nil, usagef("usage: %s %s", fs.Name(), what)
	}
	if c.output != "table" && c.output != "json" {
		return nil, usagef("--output %q: want table or json", c.output)
	}
	transport := c.transport
	chosen := c.getenv("SILO_ENDPOINT") != ""
	fs.Visit(func(f *flag.Flag) { chosen = chosen || f.Name == "endpoint" })
	if c.useFrontDoor(context.Background(), &c.front, chosen) {
		fd, err := c.newFrontDoor(&c.front)
		if err != nil {
			return nil, err
		}
		// The front door serves the same paths: the endpoint's path is
		// kept and its host is the tunnel.
		c.door, transport = fd, fd
	}
	api, err := client.New(client.Options{Endpoint: c.endpoint, Transport: transport, Timeout: c.timeout, UserAgent: "silo-cli"})
	if err != nil {
		return nil, usageError{err.Error()}
	}
	c.api = api
	return pos, nil
}

// arg is pos[i], or def if there are fewer.
func arg(pos []string, i int, def string) string {
	if i < len(pos) {
		return pos[i]
	}
	return def
}
