// Package fenceapi is the silo/v1 fence API: the tree, allocations,
// policies, members and audit, served over HTTP with JSON bodies
// (docs/silo/fence-api.md, SV-1 to SV-12, SV-13 to SV-16 (exports and
// links), SV-17 to SV-22 (manage grants), SV-23 and SV-24 (agents),
// SV-25 and SV-26; docs/silo/custom-allocators.md, SV-123;
// docs/silo/agents.md, SV-98 to SV-101, SV-104 and SV-111, the API's
// side).
//
// The transport decides who is calling: the gateway from origin
// identity, the front door from the member table, or a harness's
// session socket. Each hands the handler a Caller, through
// WithCallerFunc or ContextWithCaller; this package only checks what
// that caller may do.
package fenceapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// How a call arrived, as the transport says.
const (
	ViaOrigin    = "origin"     // from a VM in the silo, by origin identity at the gateway
	ViaFrontDoor = "front_door" // from a member, at the front door
	ViaHarness   = "harness"    // from an agent's harness, on its session socket
	ViaInternal  = "internal"   // from silod itself, or a test
)

// Caller is who is making a call: always a silo, plus how the call got
// here, which audit records as actor and via.
type Caller struct {
	Silo   string // the caller's silo id
	Via    string // ViaOrigin, ViaFrontDoor, ViaHarness or ViaInternal
	Member string // the member identity, for a front-door call
	// Agent is the agent whose session made a harness call, which the
	// session socket fills in. Objects such a call creates record it as
	// their owner_agent (call 43), and it may not renew its own lease
	// or raise its own budget (call 38).
	Agent string
	// Manage is the manage grant a call relies on, set by the
	// ManageChecker when the caller reaches a silo through one.
	Manage string
	// FromToolVM is the agent whose tool VM a call came from, which the
	// gateway's API port notes by the call's source address: audit
	// records it as from_tool_vm, to tell an agent's calls from other
	// VMs' in its silo. It changes no authority.
	FromToolVM string
}

type callerKey struct{}

// ContextWithCaller returns ctx carrying c, for a transport that wraps
// the handler.
func ContextWithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// FenceApplier waits for the fence to carry the change log up to seq:
// a create returns only once the child's fence rules are in place. The
// GCP backend implements it; NoopApplier is for everything else.
type FenceApplier interface {
	WaitApplied(ctx context.Context, seq uint64) error
}

// NoopApplier has nothing to apply.
type NoopApplier struct{}

// WaitApplied returns at once.
func (NoopApplier) WaitApplied(context.Context, uint64) error { return nil }

// FenceBudget is the fence's rule budget (gcp-backend.md "Rule
// budget", SV-60): asked in the transaction of a change that can add
// fence rules or ranges (a create, a new or wider block, a looser
// control policy), it compiles the policy the change would leave and
// returns an error with a Budget() (quota string, limit, want int)
// method for one past 90% of the recorded quota, which the change is
// refused with as RESOURCE_EXHAUSTED. nil checks nothing.
type FenceBudget interface {
	CheckBudget(ctx context.Context, tx store.Tx) error
}

// Op names an operation for a ManageChecker.
type Op string

// ManageChecker is the hook for manage grants (fence-api.md, "Manage
// grants"): asked when the caller is neither the target nor one of its
// ancestors, it returns the id of an active grant on the target or an
// ancestor that covers op; "" if the caller holds none, so the target
// stays NOT_FOUND to it; or a PERMISSION_DENIED *Error naming a grant
// it holds that doesn't cover op. ManageGrants is the one silod uses.
type ManageChecker interface {
	Covers(ctx context.Context, tx store.Tx, caller string, target *tree.Silo, op Op) (grant string, err error)
}

// SiloDeleter deletes what a silo holds in the backend (its facade
// objects, its grants) before its rows go, on a delete with force. It
// is a later task's; nil deletes nothing.
type SiloDeleter interface {
	DeleteContents(ctx context.Context, silo *tree.Silo) error
}

// Options configure a Server.
type Options struct {
	// Caller identifies the caller of a request that carries none in
	// its context. Nil, and with no Caller in the context, every call
	// but bootstrap and health is PERMISSION_DENIED.
	Caller func(*http.Request) (Caller, error)
	// Fence is waited on after a change the fence follows.
	Fence FenceApplier
	// Budget refuses a change that would take the fence past its rule
	// budget.
	Budget FenceBudget
	// Manage is the manage-grant hook; ManageGrants if nil.
	Manage ManageChecker
	// Deleter deletes a silo's backend objects.
	Deleter SiloDeleter
	// Instances resolves an export's instance target; nil refuses them.
	Instances InstanceResolver
	// Pricer prices a facade request, for quota:preview.
	Pricer Pricer
	// FacadeEndpoint is the facade's base URL, shown in describe.
	FacadeEndpoint string
	// Ready reports whether the store is usable: nil, or a reason
	// ("schema_mismatch") every call but health is UNAVAILABLE for.
	Ready func() string
	// SkipControl skips silod's own control-policy check, so that a test
	// can show the gateway's check holds alone (gateway.md, "Layer 1").
	// Never set outside tests.
	SkipControl bool
	// ToolVMs makes agents' tool VMs through the facade; with none,
	// creating an agent is UNAVAILABLE.
	ToolVMs ToolVMs
	// Runner runs agents' harness sessions. Nil runs none: agents stay
	// PROVISIONING until something reports their tool server connected.
	Runner agents.SessionRunner
	// Credentials are more agent credentials than those stored, as
	// API keys with no cap (silod's --agent-credentials, for
	// development); nil has none.
	Credentials agents.Credentials
	// Ambient is set when agents' harnesses run on silod's own host with
	// the agent user's ambient permissions (local-backend.md §10, call
	// L17): every provider then has an ambient credential,
	// ambient-<provider>, which holds no secret and is metered by the
	// usage its harness reports.
	Ambient bool
	// Usage, if set, is the host resources silos and agents have used
	// (local-backend.md §11, "Metering"), which describe and an
	// agent's get show as usage.
	Usage UsageMeter
	// CredentialKey is silo-credential-key, the AES-256 key the stored
	// credentials are sealed with; without it none can be added or used.
	CredentialKey []byte
	// Repos keeps silo repos on disk (local-git.md); with none, creating
	// one is FAILED_PRECONDITION.
	Repos RepoHost
	// Git serves git-remote-silo's connections at the front door; nil
	// refuses them.
	Git GitConnector
	// Mirror is the outbound mirror into the uber repo (local-git.md
	// §8); nil answers its calls FAILED_PRECONDITION and deletes repos
	// at once.
	Mirror Mirror
}

// Server serves silo/v1.
type Server struct {
	st   store.Store
	opts Options
	wg   sync.WaitGroup
	// running holds the delete operations in progress here.
	running sync.Map
	bg      context.Context
	stop    context.CancelFunc
	// sessions wakes whoever follows a session or waits on an answer.
	sessions notifier
}

// New returns a Server on st.
func New(st store.Store, opts Options) *Server {
	if opts.Fence == nil {
		opts.Fence = NoopApplier{}
	}
	if opts.Manage == nil {
		opts.Manage = ManageGrants{}
	}
	if opts.FacadeEndpoint == "" {
		opts.FacadeEndpoint = "http://silo.internal:8080/compute/v1/"
	}
	bg, stop := context.WithCancel(context.Background())
	return &Server{st: st, opts: opts, bg: bg, stop: stop}
}

// Close stops background work (deletions in progress stop between
// silos, and resume on the next delete call) and waits for it.
func (s *Server) Close() {
	s.stop()
	s.wg.Wait()
}

// RequestTTL is how long a request_id's first result is replayed.
const RequestTTL = 24 * time.Hour

// requestPrefix keys this API's request ids in the store's requests
// table, apart from anyone else's.
const requestPrefix = "silo/v1:"

// errValidateOnly rolls back a validate_only call's transaction.
var errValidateOnly = errors.New("validate_only")

// request is one call being served.
type request struct {
	w            http.ResponseWriter
	r            *http.Request
	caller       Caller
	validateOnly bool
	requestID    string
	target       *noted // set once a mutation has resolved its silo
}

// ServeHTTP routes a call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/silod/health":
		s.health(w, r)
		return
	case path == "/silod/bootstrap":
		if why := s.notReady(); why != "" {
			writeError(w, errorf(Unavailable, "silod is not serving: %s", why))
			return
		}
		s.bootstrap(w, r)
		return
	case path == "/silod/credentials" || strings.HasPrefix(path, "/silod/credentials/"):
		// silo uninstall's step 3 (install.md) lists the credentials
		// here and deletes each: the same calls as /silo/v1/credentials,
		// root members only.
		path = "/silo/v1/credentials" + strings.TrimPrefix(path, "/silod/credentials")
		r.URL.Path, r.URL.RawPath = path, ""
	}
	rest, ok := strings.CutPrefix(path, "/silo/v1/")
	if !ok {
		writeError(w, errorf(NotFound, "no such path %s", path))
		return
	}
	if why := s.notReady(); why != "" {
		writeError(w, errorf(Unavailable, "silod is not serving: %s", why))
		return
	}
	if reserved(rest) {
		writeError(w, errorf(Unimplemented, "%s is reserved for custom allocators, which come after v1", path).with("path", path))
		return
	}
	q := &request{w: w, r: r}
	c, ok := r.Context().Value(callerKey{}).(Caller)
	if !ok && s.opts.Caller != nil {
		var err error
		if c, err = s.opts.Caller(r); err != nil {
			writeError(w, errorf(PermissionDenied, "%v", err))
			return
		}
		ok = c.Silo != ""
	}
	if !ok {
		writeError(w, errorf(PermissionDenied, "the call carries no caller"))
		return
	}
	q.caller = c
	q.validateOnly = r.URL.Query().Get("validate_only") == "true"
	if reconcilePath(rest) {
		s.reconcile(q, rest)
		return
	}
	if mirrorPath(rest) {
		s.mirror(q, rest)
		return
	}
	if cachePath(rest) {
		s.registryCache(q)
		return
	}
	if rest == "mirror:which" {
		s.which(q)
		return
	}
	call := control.ParseFence(r.Method, rest)
	if !s.opts.SkipControl {
		if err := s.checkControl(q, call); err != nil {
			writeError(w, err)
			return
		}
	}
	s.route(q, rest, call)
}

func (s *Server) notReady() string {
	if s.opts.Ready == nil {
		return ""
	}
	return s.opts.Ready()
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if why := s.notReady(); why != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": why})
		return
	}
	// root says whether bootstrap is still open, for install's root
	// step, which may call health before it is anyone's member.
	var rooted bool
	err := s.st.Tx(r.Context(), func(tx store.Tx) error {
		_, err := tree.Root(r.Context(), tx)
		if errors.Is(err, tree.ErrNotFound) {
			return nil
		}
		rooted = err == nil
		return err
	})
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "store: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "root": rooted})
}

// reserved reports whether rest (the path after /silo/v1/) is one of
// the reserved paths, under self, a silo, or at the top.
func reserved(rest string) bool {
	segs := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	if strings.SplitN(segs[0], ":", 2)[0] == "allocators" {
		return true
	}
	if segs[0] != "self" && segs[0] != "silos" {
		return false
	}
	for _, seg := range segs[1:] {
		if control.AllocatorCollections[strings.SplitN(seg, ":", 2)[0]] {
			return true
		}
	}
	return false
}

// handlers serves each fence operation the table in pkg/control
// names. A test checks that every one here is in the table.
var handlers = map[string]func(s *Server, q *request, c control.FenceCall){
	control.OpQuotaPreview:  func(s *Server, q *request, _ control.FenceCall) { s.mutating(q, s.preview, false) },
	control.OpOperationsGet: func(s *Server, q *request, c control.FenceCall) { s.getOperation(q, c.ID) },
	control.OpSiloGet:       func(s *Server, q *request, c control.FenceCall) { s.describe(q, c.Ref) },
	control.OpSiloPatch: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.patch(q, c.Ref) }, true)
	},
	control.OpSiloDelete: func(s *Server, q *request, c control.FenceCall) { s.delete(q, c.Ref) },
	control.OpSiloGrow: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.grow(q, c.Ref) }, true)
	},
	control.OpSiloAddBlock: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.addBlock(q, c.Ref) }, true)
	},
	control.OpSiloReleaseBlock: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.releaseBlock(q, c.Ref) }, true)
	},
	control.OpChildrenList: func(s *Server, q *request, c control.FenceCall) { s.children(q, c.Ref) },
	control.OpChildrenCreate: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.create(q, c.Ref) }, true)
	},
	control.OpMembersList: func(s *Server, q *request, c control.FenceCall) { s.listMembers(q, c.Ref) },
	control.OpMembersGet:  func(s *Server, q *request, c control.FenceCall) { s.getMember(q, c.Ref, c.ID) },
	control.OpMembersPut: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.putMember(q, c.Ref, c.ID) }, true)
	},
	control.OpMembersDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.deleteMember(q, c.Ref, c.ID) }, true)
	},
	control.OpAuditList:        func(s *Server, q *request, c control.FenceCall) { s.audit(q, c.Ref) },
	control.OpManageGrantsList: func(s *Server, q *request, c control.FenceCall) { s.listManage(q, c.Ref) },
	control.OpManageGrantsCreate: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.offerManage(q, c.Ref) }, true)
	},
	control.OpManageGrantsDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.revokeManage(q, c.Ref, c.ID) }, true)
	},
	control.OpSelfManageGrantsList: func(s *Server, q *request, _ control.FenceCall) { s.listHeld(q) },
	control.OpSelfManageGrantsAccept: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.acceptManage(q, c.ID) }, true)
	},
	control.OpSelfManageGrantsRenew: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.renewManage(q, c.ID) }, true)
	},
	control.OpSelfManageGrantsDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.giveUpManage(q, c.ID) }, true)
	},
	control.OpAgentsList: func(s *Server, q *request, c control.FenceCall) { s.listAgents(q, c.Ref) },
	control.OpAgentsCreate: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.createAgent(q, c.Ref) }, true)
	},
	control.OpAgentsGet:    func(s *Server, q *request, c control.FenceCall) { s.getAgent(q, c.Ref, c.ID) },
	control.OpAgentGetByID: func(s *Server, q *request, c control.FenceCall) { s.getAgent(q, "", c.ID) },
	control.OpAgentsPatch: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.patchAgent(q, c.Ref, c.ID) }, true)
	},
	control.OpAgentsDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.deleteAgent(q, c.Ref, c.ID) }, true)
	},
	control.OpAgentsStop: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.stopAgent(q, c.Ref, c.ID) }, true)
	},
	control.OpAgentsResume: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.resumeAgent(q, c.Ref, c.ID) }, true)
	},
	control.OpAgentsRenew: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.renewAgent(q, c.Ref, c.ID) }, true)
	},
	control.OpAgentsSessionGet:   func(s *Server, q *request, c control.FenceCall) { s.getSession(q, c.Ref, c.ID) },
	control.OpAgentsMessagesList: func(s *Server, q *request, c control.FenceCall) { s.listMessages(q, c.Ref, c.ID) },
	control.OpAgentsMessagesCreate: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.postMessage(q, c.Ref, c.ID) }, true)
	},
	control.OpAgentsRequestsList: func(s *Server, q *request, c control.FenceCall) { s.listRequests(q, c.Ref, c.ID) },
	control.OpAgentsRequestsAnswer: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.answerRequest(q, c.Ref, c.ID, c.Item) }, true)
	},
	control.OpCredentialsList: func(s *Server, q *request, _ control.FenceCall) { s.listCredentials(q) },
	control.OpCredentialsAdd: func(s *Server, q *request, _ control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.addCredential(q) }, false)
	},
	control.OpCredentialsDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.removeCredential(q, c.ID) }, true)
	},
	control.OpCredentialsDeleteAll: func(s *Server, q *request, _ control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.removeAllCredentials(q) }, true)
	},
	control.OpModelPricesList: func(s *Server, q *request, _ control.FenceCall) { s.listModelPrices(q) },
	control.OpModelPricesSet: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.setModelPrice(q, c.Collection, c.ID) }, true)
	},
	control.OpModelPricesDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.deleteModelPrice(q, c.Collection, c.ID) }, true)
	},
	control.OpExportsList: func(s *Server, q *request, c control.FenceCall) { s.listGrants(q, c.Ref, "exports") },
	control.OpImportsList: func(s *Server, q *request, c control.FenceCall) { s.listGrants(q, c.Ref, "imports") },
	control.OpOffersList:  func(s *Server, q *request, c control.FenceCall) { s.listGrants(q, c.Ref, "offers") },
	control.OpExportsCreate: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.offerExport(q, c.Ref) }, true)
	},
	control.OpExportsDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.withdrawExport(q, c.Ref, c.ID, "") }, true)
	},
	control.OpExportsWithdrawTo: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.withdrawExport(q, c.Ref, c.ID, c.To) }, true)
	},
	control.OpImportsCreate: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.acceptExport(q, c.Ref) }, true)
	},
	control.OpImportsDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.deleteImport(q, c.Ref, c.ID) }, true)
	},
	control.OpLinksList: func(s *Server, q *request, c control.FenceCall) { s.listLinks(q, c.Ref) },
	control.OpLinksCreate: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.proposeLink(q, c.Ref) }, true)
	},
	control.OpLinksDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.deleteLink(q, c.Ref, c.ID) }, true)
	},
	control.OpReposList: func(s *Server, q *request, c control.FenceCall) { s.listRepos(q, c.Ref) },
	control.OpReposGet:  func(s *Server, q *request, c control.FenceCall) { s.getRepo(q, c.Ref, c.ID) },
	control.OpReposCreate: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.createRepo(q, c.Ref) }, true)
	},
	control.OpReposPatch: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.patchRepo(q, c.Ref, c.ID) }, true)
	},
	control.OpReposDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.deleteRepo(q, c.Ref, c.ID) }, true)
	},
	control.OpReposArchives: func(s *Server, q *request, c control.FenceCall) { s.listArchives(q, c.Ref, c.ID) },
	control.OpReposRestoreRef: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.restoreRef(q, c.Ref, c.ID) }, true)
	},
	control.OpReposGit:       func(s *Server, q *request, c control.FenceCall) { s.gitConnect(q, c.Ref, c.ID) },
	control.OpReposExternal:  func(s *Server, q *request, c control.FenceCall) { s.externalRepo(q, c.Ref, c.ID) },
	control.OpImageReposList: func(s *Server, q *request, c control.FenceCall) { s.listImageRepos(q, c.Ref) },
	control.OpImageReposGet:  func(s *Server, q *request, c control.FenceCall) { s.getImageRepo(q, c.Ref, c.ID) },
	control.OpImageReposTags: func(s *Server, q *request, c control.FenceCall) { s.imageRepoTags(q, c.Ref, c.ID) },
	control.OpImageReposUntag: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.untagImage(q, c.Ref, c.ID, c.Item) }, true)
	},
	control.OpImageReposDeleteManifest: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.deleteImageManifest(q, c.Ref, c.ID, c.Item) }, true)
	},
	control.OpImageReposDelete: func(s *Server, q *request, c control.FenceCall) {
		s.mutating(q, func(q *request) (any, error) { return s.deleteImageRepo(q, c.Ref, c.ID) }, true)
	},
}

// builtCollections are the sub-resources served so far; the rest of
// control.Collections are UNIMPLEMENTED.
var builtCollections = map[string]bool{"children": true, "members": true, "audit": true, "manageGrants": true,
	"exports": true, "imports": true, "offers": true, "links": true, "agents": true, "repos": true, "imageRepos": true}

// route dispatches c, the parsed call to rest, the path after
// /silo/v1/.
func (s *Server) route(q *request, rest string, c control.FenceCall) {
	if h := handlers[c.Op]; h != nil && !c.Extra {
		h(s, q, c)
		return
	}
	switch {
	case !c.OnSilo:
		writeError(q.w, errorf(NotFound, "no such path /silo/v1/%s", rest))
	case c.OnSilo && c.Ref == "":
		writeError(q.w, errorf(InvalidArgument, "no silo named in /silo/v1/%s", rest))
	case c.Collection != "" && !builtCollections[c.Collection]:
		writeError(q.w, errorf(Unimplemented, "%s is not built yet", c.Collection))
	default:
		writeError(q.w, errorf(NotFound, "no such method %s /silo/v1/%s", q.r.Method, rest))
	}
}

// resolve finds the silo ref names and checks the caller may see it.
// A silo it may not see is NOT_FOUND, exactly like one that doesn't
// exist. It returns the silo's ancestry, root first.
//
// A caller that reaches the silo through a manage grant has c.Manage
// set to the grant, which audit records as via=manage:<holder>.
func (s *Server) resolve(ctx context.Context, tx store.Tx, c *Caller, ref string, op Op) ([]*tree.Silo, relation, error) {
	var t *tree.Silo
	var err error
	switch {
	case ref == "self":
		t, err = tree.Get(ctx, tx, c.Silo)
	case strings.HasPrefix(ref, "-"):
		t, err = tree.Get(ctx, tx, ref[1:])
	default:
		t, err = tree.ByPath(ctx, tx, "/"+strings.Trim(ref, "/"))
	}
	shown := ref
	if ref != "self" && !strings.HasPrefix(ref, "-") {
		shown = "/" + strings.Trim(ref, "/")
	}
	if errors.Is(err, tree.ErrNotFound) {
		return nil, relNone, notFound(shown)
	}
	if err != nil {
		return nil, relNone, err
	}
	chain, err := tree.Ancestry(ctx, tx, t.ID)
	if err != nil {
		return nil, relNone, err
	}
	rel := relNone
	if t.ID == c.Silo {
		rel = relSelf
	} else {
		for _, a := range chain[:len(chain)-1] {
			if a.ID == c.Silo {
				rel = relAncestor
			}
		}
	}
	if rel == relNone && s.opts.Manage != nil {
		g, err := s.opts.Manage.Covers(ctx, tx, c.Silo, t, op)
		if err != nil {
			return nil, relNone, err
		}
		if g != "" {
			rel = relManage
			c.Manage = g
		}
	}
	if rel == relNone {
		return nil, relNone, notFound(shown)
	}
	return chain, rel, nil
}

type relation int

const (
	relNone relation = iota
	relSelf
	relAncestor
	relManage
)

// tx runs fn in a transaction, turning its error into an *Error while
// the transaction is still open.
func (s *Server) tx(ctx context.Context, fn func(tx store.Tx) error) error {
	var out *Error
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		// A retried attempt starts clean.
		out = nil
		err := fn(tx)
		if err != nil && !errors.Is(err, errValidateOnly) {
			out = asError(ctx, tx, err)
			return out
		}
		return err
	})
	if errors.Is(err, store.ErrConflict) {
		// Still conflicting after the store's retries.
		return &Error{Code: Unavailable, Message: err.Error()}
	}
	if out != nil {
		return out
	}
	return err
}

// stored is a request_id's recorded result.
type stored struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// mutating runs a mutation: replaying a request_id's first result,
// and, for validate_only, rolling back whatever fn did. fn runs in
// s.do's transaction and returns the response body.
func (s *Server) mutating(q *request, fn func(q *request) (any, error), record bool) {
	body, err := readBody(q.r)
	if err != nil {
		writeError(q.w, err)
		return
	}
	q.requestID, _ = body["request_id"].(string)
	if q.requestID == "" {
		q.requestID = q.r.URL.Query().Get("request_id")
	}
	q.r = q.r.WithContext(context.WithValue(q.r.Context(), bodyKey{}, body))
	if q.requestID != "" && record && !q.validateOnly {
		if res, ok, err := s.replay(q); err != nil {
			writeError(q.w, asError(q.r.Context(), nil, err))
			return
		} else if ok {
			q.w.Header().Set("Silo-Replayed", "true")
			writeRaw(q.w, res.Status, res.Body)
			return
		}
	}
	out, err := fn(q)
	if err != nil {
		e := asError(q.r.Context(), nil, err)
		if q.target != nil && !q.validateOnly && e.Code != Unavailable && e.Code != Internal {
			s.auditRefusal(q, e)
		}
		writeError(q.w, e)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// replay finds a request_id's first result, within RequestTTL.
func (s *Server) replay(q *request) (*stored, bool, error) {
	var res *stored
	err := s.st.Tx(q.r.Context(), func(tx store.Tx) error {
		key := s.requestKey(q)
		raw, ok, err := tx.Request(q.r.Context(), key)
		if err != nil || !ok {
			return err
		}
		var at int64
		if err := tx.QueryRow(q.r.Context(), `SELECT at FROM requests WHERE request_id = ?`, key).Scan(&at); err != nil {
			return err
		}
		now, err := tx.Now(q.r.Context())
		if err != nil {
			return err
		}
		if now.Sub(store.Time(at)) > RequestTTL {
			_, err := tx.Exec(q.r.Context(), `DELETE FROM requests WHERE request_id = ?`, key)
			return err
		}
		res = &stored{}
		return json.Unmarshal([]byte(raw), res)
	})
	return res, res != nil, err
}

func (s *Server) requestKey(q *request) string {
	return requestPrefix + q.caller.Silo + ":" + q.requestID
}

// finish ends a mutation's transaction: a validate_only call rolls
// back, and anything else records its result under its request_id.
func (s *Server) finish(ctx context.Context, tx store.Tx, q *request, out any) error {
	if q.validateOnly {
		return errValidateOnly
	}
	if q.requestID == "" {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	res, _ := json.Marshal(stored{Status: http.StatusOK, Body: b})
	return tx.RecordRequest(ctx, s.requestKey(q), string(res))
}

type bodyKey struct{}

func readBody(r *http.Request) (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, errorf(InvalidArgument, "reading the body: %v", err)
	}
	m := map[string]any{}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, errorf(InvalidArgument, "the body is not a JSON object: %v", err)
		}
	}
	if m == nil { // a body of null
		m = map[string]any{}
	}
	m["\x00raw"] = string(b)
	return m, nil
}

// decode reads the call's body into v. Fields v doesn't have are
// ignored (fence-api.md, "Versioning").
func decode(q *request, v any) error {
	m, _ := q.r.Context().Value(bodyKey{}).(map[string]any)
	raw, _ := m["\x00raw"].(string)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return errorf(InvalidArgument, "bad request body: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, errorf(Internal, "encoding the response: %v", err))
		return
	}
	writeRaw(w, status, b)
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, err error) {
	e := asError(context.Background(), nil, err)
	writeJSON(w, e.Code.Status(), map[string]any{"error": e})
}

// page reads page_size and page_token. A token is an offset, opaque to
// the client.
func page(q *request) (offset, size int, err error) {
	size = 100
	if v := q.r.URL.Query().Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, errorf(InvalidArgument, "page_size %q: want a non-negative integer", v)
		}
		if n > 0 {
			size = min(n, 1000)
		}
	}
	if t := q.r.URL.Query().Get("page_token"); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		n, err2 := strconv.Atoi(strings.TrimPrefix(string(b), "o"))
		if err != nil || err2 != nil || !strings.HasPrefix(string(b), "o") || n < 0 {
			return 0, 0, errorf(InvalidArgument, "bad page_token")
		}
		offset = n
	}
	return offset, size, nil
}

func nextToken(offset, size int, more bool) string {
	if !more {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("o%d", offset+size)))
}

// auditRefusal records a refused mutation of a silo the caller could
// see, in a transaction of its own since the call's rolled back.
func (s *Server) auditRefusal(q *request, e *Error) {
	ctx := q.r.Context()
	_ = s.st.Tx(ctx, func(tx store.Tx) error {
		t, err := tree.Get(ctx, tx, q.target.silo.ID)
		if err != nil {
			return nil // a create's parent, or a silo, gone since
		}
		return Audit(ctx, tx, t, q.caller, q.target.kind, q.requestID, string(e.Code), map[string]any{"message": e.Message})
	})
}

// CallerFromContext is the Caller a transport put in ctx with
// ContextWithCaller, for the facade, which shares silod's transports.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}
