package gce

import (
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Ref
	}{
		{"builder", Ref{Name: "builder"}},
		{"https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instances/vm",
			Ref{Project: "p", Zone: "us-central1-a", Collection: "instances", Name: "vm"}},
		{"https://compute.googleapis.com/compute/v1/projects/p/regions/r/subnetworks/s",
			Ref{Project: "p", Region: "r", Collection: "subnetworks", Name: "s"}},
		{"http://silo.internal:8080/compute/v1/projects/silo-k7/global/networks/default",
			Ref{Project: "silo-k7", Global: true, Collection: "networks", Name: "default"}},
		{"zones/z/machineTypes/n1-standard-4", Ref{Zone: "z", Collection: "machineTypes", Name: "n1-standard-4"}},
		{"global/instanceTemplates/t", Ref{Global: true, Collection: "instanceTemplates", Name: "t"}},
	} {
		if got := ParseRef(c.in); got != c.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	if ZoneRegion("europe-west4-a") != "europe-west4" {
		t.Error("ZoneRegion")
	}
}

func TestValidName(t *testing.T) {
	if !ValidName(strings.Repeat("a", 63)) || ValidName(strings.Repeat("a", 64)) {
		t.Error("63-character limit")
	}
	for _, bad := range []string{"", "A", "1a", "a-", "a_b"} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q)", bad)
		}
	}
}

func TestListFilterString(t *testing.T) {
	lf := ListFilter{Labels: map[string]string{"silo": "k7", "a": "b"}, NamePrefix: "k7-"}
	if got, want := lf.String(), `(labels.a eq "b") (labels.silo eq "k7") (name eq "k7-.*")`; got != want {
		t.Fatalf("String() = %s, want %s", got, want)
	}
	back, err := ParseListFilter(lf.String())
	if err != nil || back.String() != lf.String() {
		t.Fatalf("round trip %+v %v", back, err)
	}
	if !lf.Match("k7-x", map[string]string{"silo": "k7", "a": "b", "c": "d"}) || lf.Match("k7-x", map[string]string{"silo": "k7"}) {
		t.Fatal("Match")
	}
}
