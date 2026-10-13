// Package control is the control policy's shared half: the policies
// and their order, and the one table that classifies every fence API
// operation and every facade method as a read or a mutation
// (docs/silo/fence-api.md, "Control policy", SV-12; docs/silo/gateway.md,
// "Layer 1", SV-80). silod checks its callers with it, and the gateway
// imports it, so the two can't disagree about a method.
//
// A call is classified by its method, not its HTTP verb: quota:preview
// and zoneOperations.wait are POSTs and are reads. Anything the table
// doesn't know is a mutation, so a method added later is refused under
// read until the table says otherwise.
package control

import (
	"net/http"
	"strings"
)

// The control policies, strictest first.
const (
	None = "none" // sealed: no control-plane call at all
	Read = "read" // reads only
	Full = "full" // whatever the caller's place in the tree allows
)

var rank = map[string]int{None: 0, Read: 1, Full: 2}

// Valid reports whether p is a control policy.
func Valid(p string) bool { _, ok := rank[p]; return ok }

// Stricter reports whether a is stricter than b. An unknown policy is
// the strictest of all.
func Stricter(a, b string) bool { return rankOf(a) < rankOf(b) }

// Strictest is the stricter of a and b.
func Strictest(a, b string) string {
	if Stricter(b, a) {
		return b
	}
	return a
}

func rankOf(p string) int {
	if r, ok := rank[p]; ok {
		return r
	}
	return -1
}

// Class is what a call does: reads, or changes something.
type Class int

const (
	// Mutation is the zero Class, so anything unclassified is one.
	Mutation Class = iota
	Reading
)

func (c Class) String() string {
	if c == Reading {
		return "read"
	}
	return "mutation"
}

// Allows reports whether a caller whose effective policy is p may make
// a call of class c. An unknown policy allows nothing.
func Allows(p string, c Class) bool {
	switch p {
	case Full:
		return true
	case Read:
		return c == Reading
	}
	return false
}

// The fence API's operations, by name. Later tasks add theirs here,
// with their class, and route them in silod by the same name.
const (
	OpSiloGet          = "silos.get"
	OpSiloPatch        = "silos.patch"
	OpSiloDelete       = "silos.delete"
	OpSiloGrow         = "silos.grow"
	OpSiloAddBlock     = "silos.addBlock"
	OpSiloReleaseBlock = "silos.releaseBlock"
	OpChildrenList     = "silos.children.list"
	OpChildrenCreate   = "silos.children.create"
	OpMembersList      = "silos.members.list"
	OpMembersGet       = "silos.members.get"
	OpMembersPut       = "silos.members.put"
	OpMembersDelete    = "silos.members.delete"
	OpAuditList        = "silos.audit.list"
	OpQuotaPreview     = "quota.preview"
	OpOperationsGet    = "operations.get"
	// manage grants (fence-api.md, "Manage grants", SV-21): the
	// target's side, under silos/{target}/manageGrants, and the
	// holder's, under self/manageGrants.
	OpManageGrantsList       = "silos.manageGrants.list"
	OpManageGrantsCreate     = "silos.manageGrants.create"
	OpManageGrantsDelete     = "silos.manageGrants.delete"
	OpSelfManageGrantsList   = "self.manageGrants.list"
	OpSelfManageGrantsAccept = "self.manageGrants.accept"
	OpSelfManageGrantsRenew  = "self.manageGrants.renew"
	OpSelfManageGrantsDelete = "self.manageGrants.delete"
	// agents (fence-api.md, "Agents", SV-24), under
	// silos/{silo}/agents, and one by id at agents/-{id}.
	OpAgentsList   = "silos.agents.list"
	OpAgentsCreate = "silos.agents.create"
	OpAgentsGet    = "silos.agents.get"
	OpAgentsPatch  = "silos.agents.patch"
	OpAgentsDelete = "silos.agents.delete"
	OpAgentsStop   = "silos.agents.stop"
	OpAgentsResume = "silos.agents.resume"
	OpAgentsRenew  = "silos.agents.renew"
	OpAgentGetByID = "agents.get"
	// the agent control surface (agents.md, §8, SV-112), under
	// silos/{silo}/agents/{agent}: session, messages, requests and
	// requests/{id}:answer.
	OpAgentsSessionGet     = "silos.agents.session.get"
	OpAgentsMessagesList   = "silos.agents.messages.list"
	OpAgentsMessagesCreate = "silos.agents.messages.create"
	OpAgentsRequestsList   = "silos.agents.requests.list"
	OpAgentsRequestsAnswer = "silos.agents.requests.answer"
	// the model price table's overrides (agents.md, "Prices", SV-111),
	// at modelPrices and modelPrices/{provider}/{model}.
	OpModelPricesList   = "modelPrices.list"
	OpModelPricesSet    = "modelPrices.set"
	OpModelPricesDelete = "modelPrices.delete"
	// The deployment's agent credentials (agents.md, §6, SV-108), root
	// members only, at credentials and credentials/{name}.
	OpCredentialsList      = "credentials.list"
	OpCredentialsAdd       = "credentials.add"
	OpCredentialsDelete    = "credentials.delete"
	OpCredentialsDeleteAll = "credentials.deleteAll"

	// Exports (fence-api.md, "Exports (proxied)", SV-14). Each is served
	// under self and, on a descendant's behalf, under silos/{silo}
	// (SV-16), with the same op.
	OpExportsList       = "silos.exports.list"
	OpExportsCreate     = "silos.exports.create"
	OpExportsDelete     = "silos.exports.delete"
	OpExportsWithdrawTo = "silos.exports.withdrawTo"
	OpImportsList       = "silos.imports.list"
	OpImportsCreate     = "silos.imports.create"
	OpImportsDelete     = "silos.imports.delete"
	OpOffersList        = "silos.offers.list"
	// Links (fence-api.md, "Links (L3)", SV-15), likewise under self and
	// silos/{silo}.
	OpLinksList   = "silos.links.list"
	OpLinksCreate = "silos.links.create"
	OpLinksDelete = "silos.links.delete"
	// Repos (local-git.md, §2 "Operations", call L41), under
	// silos/{silo}/repos.
	OpReposList   = "silos.repos.list"
	OpReposCreate = "silos.repos.create"
	OpReposGet    = "silos.repos.get"
	OpReposPatch  = "silos.repos.patch"
	OpReposDelete = "silos.repos.delete"
)

// fenceOps classifies every fence operation.
var fenceOps = map[string]Class{
	OpSiloGet:          Reading,
	OpSiloPatch:        Mutation,
	OpSiloDelete:       Mutation,
	OpSiloGrow:         Mutation,
	OpSiloAddBlock:     Mutation,
	OpSiloReleaseBlock: Mutation,
	OpChildrenList:     Reading,
	OpChildrenCreate:   Mutation,
	OpMembersList:      Reading,
	OpMembersGet:       Reading,
	OpMembersPut:       Mutation,
	OpMembersDelete:    Mutation,
	OpAuditList:        Reading,
	OpQuotaPreview:     Reading,
	OpOperationsGet:    Reading,

	OpManageGrantsList:       Reading,
	OpManageGrantsCreate:     Mutation,
	OpManageGrantsDelete:     Mutation,
	OpSelfManageGrantsList:   Reading,
	OpSelfManageGrantsAccept: Mutation,
	OpSelfManageGrantsRenew:  Mutation,
	OpSelfManageGrantsDelete: Mutation,

	OpAgentsList:   Reading,
	OpAgentsCreate: Mutation,
	OpAgentsGet:    Reading,
	OpAgentsPatch:  Mutation,
	OpAgentsDelete: Mutation,
	OpAgentsStop:   Mutation,
	OpAgentsResume: Mutation,
	OpAgentsRenew:  Mutation,
	OpAgentGetByID: Reading,

	OpAgentsSessionGet:     Reading,
	OpAgentsMessagesList:   Reading,
	OpAgentsMessagesCreate: Mutation,
	OpAgentsRequestsList:   Reading,
	OpAgentsRequestsAnswer: Mutation,

	OpModelPricesList:      Reading,
	OpModelPricesSet:       Mutation,
	OpModelPricesDelete:    Mutation,
	OpCredentialsList:      Reading,
	OpCredentialsAdd:       Mutation,
	OpCredentialsDelete:    Mutation,
	OpCredentialsDeleteAll: Mutation,

	OpExportsList:       Reading,
	OpExportsCreate:     Mutation,
	OpExportsDelete:     Mutation,
	OpExportsWithdrawTo: Mutation,
	OpImportsList:       Reading,
	OpImportsCreate:     Mutation,
	OpImportsDelete:     Mutation,
	OpOffersList:        Reading,

	OpLinksList:   Reading,
	OpLinksCreate: Mutation,
	OpLinksDelete: Mutation,

	OpReposList:   Reading,
	OpReposCreate: Mutation,
	OpReposGet:    Reading,
	OpReposPatch:  Mutation,
	OpReposDelete: Mutation,
}

// FenceOps returns every fence operation the table knows, with its
// class.
func FenceOps() map[string]Class {
	out := make(map[string]Class, len(fenceOps))
	for k, v := range fenceOps {
		out[k] = v
	}
	return out
}

// agentSurface are the operations of the control surface of a silo's
// agents: status, sessions, messages and answers.
var agentSurface = map[string]bool{
	OpAgentsList: true, OpAgentsGet: true, OpAgentGetByID: true,
	OpAgentsSessionGet: true, OpAgentsMessagesList: true, OpAgentsMessagesCreate: true,
	OpAgentsRequestsList: true, OpAgentsRequestsAnswer: true,
}

// AgentSurface reports whether op is part of the control surface of a
// silo's agents (agents.md, "Agents in a sealed silo", SV-100), which
// an agent's harness keeps, for its own silo's agents, under a control
// policy of none or read. Its lifecycle (create, stop, resume, renew,
// delete) and its budget are not part of it.
func AgentSurface(op string) bool { return agentSurface[op] }

// FenceClass classifies a fence operation; an unknown one ("" included)
// is a mutation.
func FenceClass(op string) Class { return fenceOps[op] }

// Collections are the names that, as a path segment after a silo
// reference, start a sub-resource. No silo may take one as its name
// (ReservedName). Some are not served yet; their paths parse but name
// no operation, so they are mutations.
var Collections = map[string]bool{
	"children": true, "members": true, "audit": true,
	"exports": true, "imports": true, "offers": true, "links": true, "manageGrants": true, "agents": true,
	"repos": true,
}

// AllocatorCollections are the custom-allocator collections v1
// reserves (custom-allocators.md, fence API items 6 to 11): a path
// with one as a segment is UNIMPLEMENTED.
var AllocatorCollections = map[string]bool{"allocators": true, "allocator-offers": true, "allocator-grants": true, "resources": true}

// ReservedName reports whether name, as a path segment, would be read
// as a collection rather than a silo, so no silo may take it as its
// name: a silo so named could not be addressed by path.
func ReservedName(name string) bool { return Collections[name] || AllocatorCollections[name] }

// FenceCall is a fence API path, parsed.
type FenceCall struct {
	// Op is the operation, or "" if the path and method name none.
	Op string
	// OnSilo is set when the path is about a silo (self or silos/...).
	OnSilo bool
	// Ref is the silo reference: "self", a path without its leading
	// slash, or -id.
	Ref string
	// Collection is the sub-resource the path names after the silo,
	// if any, and ID the item in it.
	Collection string
	ID         string
	// Extra is set when the path has segments past the item.
	Extra bool
	// To is the silo path of .../exports/{name}/to/{silo}.
	To string
	// Item is the item under an agent's sub-collection: the request of
	// .../agents/{agent}/requests/{id}:answer.
	Item string
}

// ParseFence parses a fence API call: its HTTP method, and its path
// after /silo/v1/.
func ParseFence(method, rest string) FenceCall {
	switch {
	case rest == "quota:preview":
		if method == http.MethodPost {
			return FenceCall{Op: OpQuotaPreview}
		}
		return FenceCall{}
	case strings.HasPrefix(rest, "operations/"):
		if method == http.MethodGet {
			return FenceCall{Op: OpOperationsGet, ID: strings.TrimPrefix(rest, "operations/")}
		}
		return FenceCall{}
	case strings.HasPrefix(rest, "agents/"):
		segs := splitPath(strings.TrimPrefix(rest, "agents/"))
		if len(segs) == 1 && strings.HasPrefix(segs[0], "-") && len(segs[0]) > 1 && method == http.MethodGet {
			return FenceCall{Op: OpAgentGetByID, ID: segs[0]}
		}
		return FenceCall{}
	case rest == "credentials" || strings.HasPrefix(rest, "credentials/"):
		segs := splitPath(strings.TrimPrefix(rest, "credentials"))
		switch {
		case len(segs) == 0 && method == http.MethodGet:
			return FenceCall{Op: OpCredentialsList}
		case len(segs) == 0 && method == http.MethodPost:
			return FenceCall{Op: OpCredentialsAdd}
		case len(segs) == 0 && method == http.MethodDelete:
			return FenceCall{Op: OpCredentialsDeleteAll}
		case len(segs) == 1 && method == http.MethodDelete:
			return FenceCall{Op: OpCredentialsDelete, ID: segs[0]}
		}
		return FenceCall{}
	case rest == "modelPrices" || strings.HasPrefix(rest, "modelPrices/"):
		segs := splitPath(strings.TrimPrefix(rest, "modelPrices"))
		switch {
		case len(segs) == 0 && method == http.MethodGet:
			return FenceCall{Op: OpModelPricesList}
		case len(segs) == 2 && method == http.MethodPut:
			return FenceCall{Op: OpModelPricesSet, Collection: segs[0], ID: segs[1]}
		case len(segs) == 2 && method == http.MethodDelete:
			return FenceCall{Op: OpModelPricesDelete, Collection: segs[0], ID: segs[1]}
		}
		return FenceCall{}
	}
	var ref string
	var segs []string
	// self is set for a path under self/ rather than silos/: the
	// holder's side of manage grants is only there.
	self := false
	switch {
	case rest == "self" || strings.HasPrefix(rest, "self/") || strings.HasPrefix(rest, "self:"):
		ref, self = "self", true
		if strings.HasPrefix(rest, "self:") {
			ref = rest
		} else {
			segs = splitPath(strings.TrimPrefix(rest, "self"))
		}
	case strings.HasPrefix(rest, "silos/"):
		all := splitPath(strings.TrimPrefix(rest, "silos/"))
		i := 0
		for i < len(all) && !Collections[all[i]] {
			i++
		}
		ref, segs = strings.Join(all[:i], "/"), all[i:]
	default:
		return FenceCall{}
	}
	verb := ""
	if len(segs) == 0 {
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			ref, verb = ref[:i], ref[i+1:]
		}
	}
	c := FenceCall{OnSilo: true, Ref: ref}
	if len(segs) > 0 {
		c.Collection = segs[0]
	}
	if len(segs) > 1 {
		c.ID = segs[1]
	}
	c.Extra = len(segs) > 2
	// DELETE .../exports/{name}/to/{silo}: the silo is a path, so the
	// rest of the segments.
	if c.Collection == "exports" && len(segs) > 3 && segs[2] == "to" {
		c.To, c.Extra = strings.Join(segs[3:], "/"), false
	}
	if ref == "" {
		return c
	}
	switch {
	case len(segs) == 0 && verb == "":
		c.Op = map[string]string{http.MethodGet: OpSiloGet, http.MethodPatch: OpSiloPatch, http.MethodDelete: OpSiloDelete}[method]
	case len(segs) == 0:
		if method == http.MethodPost {
			c.Op = map[string]string{"grow": OpSiloGrow, "addBlock": OpSiloAddBlock, "releaseBlock": OpSiloReleaseBlock}[verb]
		}
	case c.Collection == "children" && len(segs) == 1:
		c.Op = map[string]string{http.MethodGet: OpChildrenList, http.MethodPost: OpChildrenCreate}[method]
	case c.Collection == "members" && len(segs) == 1:
		if method == http.MethodGet {
			c.Op = OpMembersList
		}
	case c.Collection == "members" && len(segs) == 2:
		c.Op = map[string]string{http.MethodGet: OpMembersGet, http.MethodPut: OpMembersPut, http.MethodDelete: OpMembersDelete}[method]
	case c.Collection == "audit" && len(segs) == 1:
		if method == http.MethodGet {
			c.Op = OpAuditList
		}
	case c.Collection == "manageGrants" && self && len(segs) == 1:
		if method == http.MethodGet {
			c.Op = OpSelfManageGrantsList
		}
	case c.Collection == "manageGrants" && self && len(segs) == 2:
		id, verb, _ := strings.Cut(c.ID, ":")
		c.ID = id
		switch {
		case verb == "" && method == http.MethodDelete:
			c.Op = OpSelfManageGrantsDelete
		case verb == "accept" && method == http.MethodPost:
			c.Op = OpSelfManageGrantsAccept
		case verb == "renew" && method == http.MethodPost:
			c.Op = OpSelfManageGrantsRenew
		}
	case c.Collection == "manageGrants" && len(segs) == 1:
		c.Op = map[string]string{http.MethodGet: OpManageGrantsList, http.MethodPost: OpManageGrantsCreate}[method]
	case c.Collection == "manageGrants" && len(segs) == 2:
		if method == http.MethodDelete {
			c.Op = OpManageGrantsDelete
		}
	case c.Collection == "agents" && len(segs) == 1:
		c.Op = map[string]string{http.MethodGet: OpAgentsList, http.MethodPost: OpAgentsCreate}[method]
	case c.Collection == "agents" && len(segs) == 2:
		id, verb, _ := strings.Cut(c.ID, ":")
		c.ID = id
		if verb == "" {
			c.Op = map[string]string{http.MethodGet: OpAgentsGet, http.MethodPatch: OpAgentsPatch, http.MethodDelete: OpAgentsDelete}[method]
		} else if method == http.MethodPost {
			c.Op = map[string]string{"stop": OpAgentsStop, "resume": OpAgentsResume, "renew": OpAgentsRenew}[verb]
		}
	case c.Collection == "agents" && len(segs) == 3 && !strings.Contains(c.ID, ":"):
		c.Extra = false
		switch {
		case segs[2] == "session" && method == http.MethodGet:
			c.Op = OpAgentsSessionGet
		case segs[2] == "messages":
			c.Op = map[string]string{http.MethodGet: OpAgentsMessagesList, http.MethodPost: OpAgentsMessagesCreate}[method]
		case segs[2] == "requests" && method == http.MethodGet:
			c.Op = OpAgentsRequestsList
		default:
			c.Extra = true
		}
	case c.Collection == "agents" && len(segs) == 4 && segs[2] == "requests" && !strings.Contains(c.ID, ":"):
		if id, ok := strings.CutSuffix(segs[3], ":answer"); ok && id != "" && method == http.MethodPost {
			c.Op, c.Item, c.Extra = OpAgentsRequestsAnswer, id, false
		}
	case c.Collection == "exports" && len(segs) == 1:
		c.Op = map[string]string{http.MethodGet: OpExportsList, http.MethodPost: OpExportsCreate}[method]
	case c.Collection == "exports" && len(segs) == 2:
		if method == http.MethodDelete {
			c.Op = OpExportsDelete
		}
	case c.Collection == "exports" && c.To != "":
		if method == http.MethodDelete {
			c.Op = OpExportsWithdrawTo
		}
	case c.Collection == "imports" && len(segs) == 1:
		c.Op = map[string]string{http.MethodGet: OpImportsList, http.MethodPost: OpImportsCreate}[method]
	case c.Collection == "imports" && len(segs) == 2:
		if method == http.MethodDelete {
			c.Op = OpImportsDelete
		}
	case c.Collection == "offers" && len(segs) == 1:
		if method == http.MethodGet {
			c.Op = OpOffersList
		}
	case c.Collection == "links" && len(segs) == 1:
		c.Op = map[string]string{http.MethodGet: OpLinksList, http.MethodPost: OpLinksCreate}[method]
	case c.Collection == "links" && len(segs) == 2:
		if method == http.MethodDelete {
			c.Op = OpLinksDelete
		}
	case c.Collection == "repos" && len(segs) == 1:
		c.Op = map[string]string{http.MethodGet: OpReposList, http.MethodPost: OpReposCreate}[method]
	case c.Collection == "repos" && len(segs) == 2:
		c.Op = map[string]string{http.MethodGet: OpReposGet, http.MethodPatch: OpReposPatch, http.MethodDelete: OpReposDelete}[method]
	}
	return c
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// FenceRequestClass classifies a fence API request by its HTTP method
// and full URL path (/silo/v1/...). Anything that isn't a known fence
// operation is a mutation.
func FenceRequestClass(method, path string) Class {
	rest, ok := strings.CutPrefix(path, "/silo/v1/")
	if !ok {
		return Mutation
	}
	return FenceClass(ParseFence(method, rest).Op)
}

// The facade's read methods (fence-api.md, "Control policy"): on any
// resource, these only read.
var facadeReads = map[string]bool{
	"get": true, "list": true, "aggregatedList": true, "getFromFamily": true, "getSerialPortOutput": true,
}

// operationResources are the facade's operation collections, whose
// wait is a read too.
var operationResources = map[string]bool{
	"zoneOperations": true, "regionOperations": true, "globalOperations": true,
}

// facadeMethods are the methods the facade covers, by resource
// (gcp-backend.md, "Covered"). Every method is listed whether it reads
// or writes, so the table is the whole facade.
var facadeMethods = map[string][]string{
	"regions":                 {"get", "list"},
	"zones":                   {"get", "list"},
	"machineTypes":            {"get", "list", "aggregatedList"},
	"diskTypes":               {"get", "list", "aggregatedList"},
	"acceleratorTypes":        {"get", "list", "aggregatedList"},
	"zoneOperations":          {"get", "wait", "list"},
	"regionOperations":        {"get", "wait", "list"},
	"globalOperations":        {"get", "wait", "list"},
	"instances":               {"insert", "get", "list", "aggregatedList", "delete", "start", "stop", "reset", "suspend", "resume", "attachDisk", "detachDisk", "setLabels", "setMetadata", "setTags", "setMachineType", "setDeletionProtection", "getSerialPortOutput"},
	"disks":                   {"insert", "get", "list", "aggregatedList", "delete", "resize", "update", "setLabels", "createSnapshot"},
	"images":                  {"insert", "get", "getFromFamily", "list", "delete", "setLabels", "deprecate"},
	"snapshots":               {"get", "list", "delete", "setLabels"},
	"networks":                {"get", "list"},
	"subnetworks":             {"insert", "get", "list", "aggregatedList", "delete", "patch", "expandIpCidrRange"},
	"firewalls":               {"insert", "get", "list", "delete", "patch", "update"},
	"instanceTemplates":       {"insert", "get", "list", "delete"},
	"regionInstanceTemplates": {"insert", "get", "list", "delete"},
	"projects":                {"get"},
}

var facadeTable = func() map[string]Class {
	out := map[string]Class{}
	for res, ms := range facadeMethods {
		for _, m := range ms {
			c := Mutation
			if facadeReads[m] || (operationResources[res] && m == "wait") {
				c = Reading
			}
			out[res+"."+m] = c
		}
	}
	return out
}()

// FacadeMethods returns every facade method the table knows, as
// "resource.method" (instances.insert, zoneOperations.wait), with its
// class.
func FacadeMethods() map[string]Class {
	out := make(map[string]Class, len(facadeTable))
	for k, v := range facadeTable {
		out[k] = v
	}
	return out
}

// FacadeClass classifies a facade method named "resource.method", with
// or without a leading "compute.". A method the facade doesn't cover is
// a mutation.
func FacadeClass(method string) Class {
	return facadeTable[strings.TrimPrefix(method, "compute.")]
}
