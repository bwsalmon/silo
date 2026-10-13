package manage

import "testing"

func TestCovers(t *testing.T) {
	g := &Grant{Scope: Scope{Facade: &FacadeScope{Resources: []string{"instances", "disks"}, Access: Write}, Fence: []string{GroupRead, GroupBlocks}}}
	ro := &Grant{Scope: Scope{Facade: &FacadeScope{Resources: []string{AllResources}, Access: Read}, Fence: []string{GroupRead}}}
	empty := &Grant{Scope: Scope{Fence: []string{GroupRead}}}
	for _, c := range []struct {
		g      *Grant
		what   string
		access Access
		want   bool
	}{
		{g, "instances", Write, true},
		{g, "disks", Read, true},
		{g, "subnetworks", Read, false},
		{g, "firewalls", Write, false},
		{g, "zoneOperations", Read, true}, // following a write it made
		{g, "zones", Read, true},
		{g, "zoneOperations", Write, false},
		{g, GroupRead, "", true},
		{g, GroupBlocks, "", true},
		{g, GroupChildren, "", false},
		{g, GroupAgents, "", false},
		{ro, "subnetworks", Read, true},
		{ro, "someTypeAddedLater", Read, true},
		{ro, "instances", Write, false},
		{empty, GroupRead, "", true},
		{empty, "zones", Read, false},
		{empty, GroupGrants, "", false},
	} {
		if got := c.g.Covers(c.what, c.access); got != c.want {
			t.Errorf("%+v covers %s %s: %v, want %v", c.g.Scope, c.access, c.what, got, c.want)
		}
	}
}

func TestResourcesAndGroups(t *testing.T) {
	rs := map[string]bool{}
	for _, r := range Resources() {
		rs[r] = true
	}
	for _, r := range []string{"instances", "disks", "subnetworks", "firewalls", "images", "snapshots", "instanceTemplates", "repos", "registry"} {
		if !rs[r] {
			t.Errorf("%s is not a scope resource", r)
		}
	}
	if rs["routes"] {
		t.Error("routes, which the facade doesn't cover, is a scope resource")
	}
	for _, g := range []string{"read", "grants", "children", "blocks", "agents"} {
		if !IsGroup(g) {
			t.Errorf("%s is not a group", g)
		}
	}
	if IsGroup("control") || IsGroup("instances") {
		t.Error("IsGroup is too broad")
	}
}
