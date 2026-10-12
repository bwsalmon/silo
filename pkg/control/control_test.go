package control

import (
	"net/http"
	"testing"
)

func TestOrder(t *testing.T) {
	if !Stricter(None, Read) || !Stricter(Read, Full) || Stricter(Full, Read) || Stricter(Read, Read) {
		t.Error("want none < read < full")
	}
	if !Stricter("bogus", None) {
		t.Error("an unknown policy is the strictest")
	}
	if Strictest(Full, Read) != Read || Strictest(None, Read) != None {
		t.Error("Strictest")
	}
	for _, c := range []struct {
		p     string
		read  bool
		write bool
	}{{Full, true, true}, {Read, true, false}, {None, false, false}, {"", false, false}} {
		if Allows(c.p, Reading) != c.read || Allows(c.p, Mutation) != c.write {
			t.Errorf("Allows(%q): read %v, mutation %v", c.p, Allows(c.p, Reading), Allows(c.p, Mutation))
		}
	}
}

func TestFenceTable(t *testing.T) {
	for _, c := range []struct {
		method, path string
		op           string
		class        Class
	}{
		{"GET", "/silo/v1/self", OpSiloGet, Reading},
		{"GET", "/silo/v1/silos/team-a/ci", OpSiloGet, Reading},
		{"PATCH", "/silo/v1/silos/-abc", OpSiloPatch, Mutation},
		{"DELETE", "/silo/v1/self", OpSiloDelete, Mutation},
		{"POST", "/silo/v1/self:grow", OpSiloGrow, Mutation},
		{"POST", "/silo/v1/silos/team-a:addBlock", OpSiloAddBlock, Mutation},
		{"POST", "/silo/v1/silos/team-a:releaseBlock", OpSiloReleaseBlock, Mutation},
		{"GET", "/silo/v1/self/children", OpChildrenList, Reading},
		{"POST", "/silo/v1/silos/team-a/children", OpChildrenCreate, Mutation},
		{"GET", "/silo/v1/self/members", OpMembersList, Reading},
		{"GET", "/silo/v1/self/members/user:a@x", OpMembersGet, Reading},
		{"PUT", "/silo/v1/self/members/user:a@x", OpMembersPut, Mutation},
		{"DELETE", "/silo/v1/self/members/user:a@x", OpMembersDelete, Mutation},
		{"GET", "/silo/v1/silos/team-a/audit", OpAuditList, Reading},
		// By method, not verb: a POST that reads.
		{"POST", "/silo/v1/quota:preview", OpQuotaPreview, Reading},
		{"GET", "/silo/v1/operations/op-1", OpOperationsGet, Reading},
		{"GET", "/silo/v1/silos/prod/manageGrants", OpManageGrantsList, Reading},
		{"POST", "/silo/v1/silos/prod/manageGrants", OpManageGrantsCreate, Mutation},
		{"DELETE", "/silo/v1/silos/prod/manageGrants/rollout", OpManageGrantsDelete, Mutation},
		{"GET", "/silo/v1/self/manageGrants", OpSelfManageGrantsList, Reading},
		{"POST", "/silo/v1/self/manageGrants/g1:accept", OpSelfManageGrantsAccept, Mutation},
		{"POST", "/silo/v1/self/manageGrants/g1:renew", OpSelfManageGrantsRenew, Mutation},
		{"DELETE", "/silo/v1/self/manageGrants/g1", OpSelfManageGrantsDelete, Mutation},
		{"GET", "/silo/v1/silos/a/b/agents", OpAgentsList, Reading},
		{"POST", "/silo/v1/self/agents", OpAgentsCreate, Mutation},
		{"GET", "/silo/v1/silos/a/b/agents/fixer", OpAgentsGet, Reading},
		{"PATCH", "/silo/v1/silos/a/agents/fixer", OpAgentsPatch, Mutation},
		{"DELETE", "/silo/v1/silos/a/agents/-m3x8re", OpAgentsDelete, Mutation},
		{"POST", "/silo/v1/silos/a/agents/fixer:stop", OpAgentsStop, Mutation},
		{"POST", "/silo/v1/silos/a/agents/fixer:resume", OpAgentsResume, Mutation},
		{"POST", "/silo/v1/self/agents/fixer:renew", OpAgentsRenew, Mutation},
		{"GET", "/silo/v1/agents/-m3x8re", OpAgentGetByID, Reading},
		{"GET", "/silo/v1/modelPrices", OpModelPricesList, Reading},
		{"PUT", "/silo/v1/modelPrices/anthropic/claude-opus-5-5", OpModelPricesSet, Mutation},
		{"DELETE", "/silo/v1/modelPrices/anthropic/claude-opus-5-5", OpModelPricesDelete, Mutation},
		// The control surface.
		{"GET", "/silo/v1/silos/a/agents/fixer/session", OpAgentsSessionGet, Reading},
		{"GET", "/silo/v1/self/agents/-m3x8re/messages", OpAgentsMessagesList, Reading},
		{"POST", "/silo/v1/silos/a/agents/fixer/messages", OpAgentsMessagesCreate, Mutation},
		{"GET", "/silo/v1/silos/a/agents/fixer/requests", OpAgentsRequestsList, Reading},
		{"POST", "/silo/v1/silos/a/agents/fixer/requests/r1:answer", OpAgentsRequestsAnswer, Mutation},
		{"GET", "/silo/v1/silos/a/agents/fixer/requests/r1:answer", "", Mutation},
		{"POST", "/silo/v1/silos/a/agents/fixer/session", "", Mutation},
		{"GET", "/silo/v1/silos/a/agents/fixer:stop/session", "", Mutation},
		// The tool channel is the gateway's, not an operation.
		{"GET", "/silo/v1/agents/-/tools/binary", "", Mutation},
		{"POST", "/silo/v1/silos/a/agents/fixer:frobnicate", "", Mutation},
		// The holder's side is under self only.
		{"POST", "/silo/v1/silos/prod/manageGrants/g1:accept", "", Mutation},
		{"POST", "/silo/v1/self/manageGrants/g1:frobnicate", "", Mutation},
		{"PUT", "/silo/v1/silos/prod/manageGrants/rollout", "", Mutation},
		// Exports, under self and on a descendant's behalf.
		{"GET", "/silo/v1/self/exports", OpExportsList, Reading},
		{"POST", "/silo/v1/self/exports", OpExportsCreate, Mutation},
		{"DELETE", "/silo/v1/self/exports/pkgcache", OpExportsDelete, Mutation},
		{"DELETE", "/silo/v1/self/exports/pkgcache/to/team-a/ci", OpExportsWithdrawTo, Mutation},
		{"GET", "/silo/v1/silos/team-a/ci/imports", OpImportsList, Reading},
		{"POST", "/silo/v1/silos/team-a/ci/imports", OpImportsCreate, Mutation},
		{"DELETE", "/silo/v1/self/imports/i1", OpImportsDelete, Mutation},
		{"GET", "/silo/v1/silos/prod/offers", OpOffersList, Reading},
		{"GET", "/silo/v1/self/exports/pkgcache/to/x", "", Mutation},
		// Links, likewise.
		{"GET", "/silo/v1/self/links", OpLinksList, Reading},
		{"POST", "/silo/v1/self/links", OpLinksCreate, Mutation},
		{"DELETE", "/silo/v1/self/links/db", OpLinksDelete, Mutation},
		{"POST", "/silo/v1/silos/team-a/ci/links", OpLinksCreate, Mutation},
		{"GET", "/silo/v1/self/links/db", "", Mutation},
		// Unknown methods and paths are mutations.
		{"GET", "/silo/v1/quota:preview", "", Mutation},
		{"PUT", "/silo/v1/self/exports", "", Mutation},
		{"GET", "/silo/v1/self/children/x", "", Mutation},
		{"POST", "/silo/v1/self:frobnicate", "", Mutation},
		{"PUT", "/silo/v1/self", "", Mutation},
		{"GET", "/silo/v1/nonsense", "", Mutation},
		{"GET", "/elsewhere", "", Mutation},
	} {
		if got := FenceRequestClass(c.method, c.path); got != c.class {
			t.Errorf("%s %s: %v, want %v", c.method, c.path, got, c.class)
		}
		if rest, ok := cut(c.path); ok {
			if got := ParseFence(c.method, rest).Op; got != c.op {
				t.Errorf("%s %s: op %q, want %q", c.method, c.path, got, c.op)
			}
		}
	}
	for op := range FenceOps() {
		if FenceClass(op) != fenceOps[op] {
			t.Errorf("%s: FenceClass disagrees with the table", op)
		}
	}
}

func cut(p string) (string, bool) {
	const pre = "/silo/v1/"
	if len(p) < len(pre) || p[:len(pre)] != pre {
		return "", false
	}
	return p[len(pre):], true
}

func TestFacadeTable(t *testing.T) {
	reads := []string{
		"regions.get", "zones.list", "machineTypes.aggregatedList", "images.getFromFamily",
		"instances.get", "instances.list", "instances.aggregatedList", "instances.getSerialPortOutput",
		"disks.get", "snapshots.list", "networks.get", "subnetworks.aggregatedList", "firewalls.get",
		"instanceTemplates.list", "regionInstanceTemplates.get", "projects.get",
		"zoneOperations.get", "zoneOperations.list", "zoneOperations.wait",
		"regionOperations.wait", "globalOperations.wait", "compute.instances.get",
	}
	mutations := []string{
		"instances.insert", "instances.delete", "instances.start", "instances.stop", "instances.reset",
		"instances.suspend", "instances.resume", "instances.attachDisk", "instances.detachDisk",
		"instances.setLabels", "instances.setMetadata", "instances.setTags", "instances.setMachineType",
		"instances.setDeletionProtection", "disks.insert", "disks.resize", "disks.update", "disks.createSnapshot",
		"images.insert", "images.deprecate", "snapshots.delete", "subnetworks.patch", "subnetworks.expandIpCidrRange",
		"firewalls.update", "instanceTemplates.insert", "regionInstanceTemplates.delete", "compute.disks.delete",
		// Not covered by the facade, or unknown: mutations.
		"networks.insert", "projects.setCommonInstanceMetadata", "instances.wait", "routes.list", "", "get",
	}
	for _, m := range reads {
		if FacadeClass(m) != Reading {
			t.Errorf("%s: want a read", m)
		}
	}
	for _, m := range mutations {
		if FacadeClass(m) != Mutation {
			t.Errorf("%s: want a mutation", m)
		}
	}
	// The table is the whole of the covered facade: each read in it is
	// one of the read methods, and everything else a mutation.
	n := 0
	for m, c := range FacadeMethods() {
		n++
		if FacadeClass(m) != c {
			t.Errorf("%s: FacadeClass disagrees with FacadeMethods", m)
		}
	}
	if n < 80 {
		t.Errorf("the facade table has %d methods; gcp-backend.md covers more", n)
	}
}

func TestParseFenceParts(t *testing.T) {
	c := ParseFence(http.MethodPut, "silos/team-a/web/members/user:a@x")
	if c.Ref != "team-a/web" || c.Collection != "members" || c.ID != "user:a@x" || !c.OnSilo || c.Op != OpMembersPut {
		t.Errorf("%+v", c)
	}
	c = ParseFence(http.MethodPost, "self:grow")
	if c.Ref != "self" || c.Op != OpSiloGrow {
		t.Errorf("%+v", c)
	}
	c = ParseFence(http.MethodPost, "self/manageGrants/g1:renew")
	if c.Ref != "self" || c.Collection != "manageGrants" || c.ID != "g1" || c.Op != OpSelfManageGrantsRenew {
		t.Errorf("%+v", c)
	}
	c = ParseFence(http.MethodGet, "silos/")
	if !c.OnSilo || c.Ref != "" || c.Op != "" {
		t.Errorf("%+v", c)
	}
}
