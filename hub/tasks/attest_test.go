package tasks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/claims"
	"github.com/bwsalmon/silo/hub/identity"
)

func testSigner(t *testing.T) *claims.Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, err)
	der, err := x509.MarshalECPrivateKey(k)
	check(t, err)
	s, err := claims.NewSigner("https://hub.example.com", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	check(t, err)
	return s
}

// verifyAttestation checks m's attestation as a node's grain would: its
// signature against the published keys, its issuer, that it is for
// node, and that its digest is of the task m carries. It returns its
// claims.
func verifyAttestation(t *testing.T, s *claims.Signer, m Message, node string) claims.Claims {
	t.Helper()
	if m.Attestation == "" {
		t.Fatalf("%s without an attestation", m.Type)
	}
	keys := httptest.NewServer(s.Handler())
	defer keys.Close()
	if _, err := identity.Verify(context.Background(), &identity.KeySet{URL: keys.URL}, m.Attestation,
		identity.Expect{Issuer: s.Issuer, Audience: node}); err != nil {
		t.Fatalf("the %s's attestation: %v", m.Type, err)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.Split(m.Attestation, ".")[1])
	check(t, err)
	var c claims.Claims
	check(t, json.Unmarshal(b, &c))
	want := claims.SpecDigest(claims.Spec{Target: m.Task.Repo, Title: m.Task.Title, Body: m.Task.Prompt})
	if c.SpecDigest != want {
		t.Errorf("spec_digest %s, want %s for the task offered", c.SpecDigest, want)
	}
	if c.HubTaskID == "" || c.HubTaskID != m.Task.ID {
		t.Errorf("hub_task_id %q, want the offered task's ID %q", c.HubTaskID, m.Task.ID)
	}
	if c.IssuedAt == 0 || c.Expiry <= c.IssuedAt || c.Expiry-c.IssuedAt > int64(claims.MaxTTL.Seconds()) {
		t.Errorf("iat %d, exp %d: want both, at most a day apart", c.IssuedAt, c.Expiry)
	}
	return c
}

func TestOfferAndResumeAttested(t *testing.T) {
	s := testSigner(t)
	h := newHarness(t, func(d *Dispatcher) { d.Claims = s })
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Title: "Fix it", Prompt: "fix the parser", Submitter: "carol@example.com"})

	o := n.next(t, TypeOffer)
	c := verifyAttestation(t, s, o.first, "ws-1")
	if c.Requester != "carol@example.com" || c.Team != "infra" || c.ApprovedBy != "" || len(c.GrantedCapabilities) != 0 || c.NetworkAccess != "" {
		t.Errorf("claims %+v", c)
	}
	o.send(t, Message{Type: TypeAccept})
	h.await(t, task.ID, "running", inState(Running))

	// A resume is attested afresh, to the node it is written to.
	n.conn.Close()
	h.await(t, task.ID, "lost", inState(Lost))
	n.connect(t)
	r := n.next(t, TypeResume)
	verifyAttestation(t, s, r.first, "ws-1")
	r.send(t, Message{Type: TypeAccept})
	h.await(t, task.ID, "running again", inState(Running))
}

// TestReattested: while a node has a task, the dispatcher sends it a
// fresh attestation for the task every Reattest, which the node can
// check against the task it was offered.
func TestReattested(t *testing.T) {
	s := testSigner(t)
	h := newHarness(t, func(d *Dispatcher) { d.Claims, d.Reattest = s, 200*time.Millisecond })
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Title: "Fix it", Prompt: "fix the parser", Submitter: "carol@example.com"})

	o := n.next(t, TypeOffer)
	first := verifyAttestation(t, s, o.first, "ws-1")
	o.send(t, Message{Type: TypeAccept})
	h.await(t, task.ID, "running", inState(Running))

	last := o.first.Attestation
	for range 2 {
		m, err := o.Read()
		check(t, err)
		if m.Type != TypeAttest || m.Task == nil || m.Task.ID != task.ID {
			t.Fatalf("got %+v, want an attest for %s", m, task.ID)
		}
		if m.Attestation == last {
			t.Error("the attest repeats the last attestation")
		}
		last = m.Attestation
		// The node checks it against the task it was offered.
		m.Task = o.first.Task
		if c := verifyAttestation(t, s, m, "ws-1"); c.Requester != first.Requester || c.Team != first.Team || c.SpecDigest != first.SpecDigest {
			t.Errorf("fresh claims %+v, offered %+v", c, first)
		}
	}
	o.send(t, Message{Type: TypeResult, State: Done})
	h.await(t, task.ID, "done", inState(Done))
}

// TestOfferApprovedBy: a task approved at the hub is offered with its
// approver, and its attestation's approved_by claim names them.
func TestOfferApprovedBy(t *testing.T) {
	s := testSigner(t)
	h := newHarness(t, func(d *Dispatcher) { d.Claims = s })
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Prompt: "fix the parser", Submitter: "carol@example.com", ApprovedBy: "dave@example.com"})

	o := n.next(t, TypeOffer)
	if o.first.Task.ApprovedBy != "dave@example.com" {
		t.Errorf("offered %+v", o.first.Task)
	}
	if c := verifyAttestation(t, s, o.first, "ws-1"); c.Requester != "carol@example.com" || c.ApprovedBy != "dave@example.com" {
		t.Errorf("claims %+v", c)
	}
}

func TestOfferUnattestedWithoutKey(t *testing.T) {
	h := newHarness(t)
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	if o := n.next(t, TypeOffer); o.first.Attestation != "" {
		t.Errorf("an offer with no signer has an attestation: %q", o.first.Attestation)
	}
}
