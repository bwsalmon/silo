// Adapted from github.com/bwsalmon/grain pkg/gitproxy/refupdates_test.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

var (
	oidA = strings.Repeat("a", 40)
	oidB = strings.Repeat("b", 40)
	zero = strings.Repeat("0", 40)
)

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// pushBody is a receive-pack request as git sends one: the first command
// carries the capabilities, a flush ends the list, and the packfile
// follows.
func pushBody(commands ...string) []byte {
	var b strings.Builder
	for i, c := range commands {
		if i == 0 {
			c += "\x00report-status side-band-64k agent=git/2.39.2"
		}
		b.WriteString(pkt(c + "\n"))
	}
	b.WriteString("0000PACK\x00\x00\x00\x02not really a pack")
	return []byte(b.String())
}

func TestParseReceivePackCommandsReadsEveryCommand(t *testing.T) {
	body := pushBody(
		oidA+" "+oidB+" refs/heads/grain/task-7",
		zero+" "+oidB+" refs/heads/new",
		oidA+" "+zero+" refs/tags/v1",
	)
	updates, err := ParseReceivePackCommands(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []RefUpdate{
		{oidA, oidB, "refs/heads/grain/task-7"},
		{zero, oidB, "refs/heads/new"},
		{oidA, zero, "refs/tags/v1"},
	}
	if fmt.Sprint(updates) != fmt.Sprint(want) {
		t.Errorf("updates = %v, want %v", updates, want)
	}
}

func TestParseReceivePackCommandsSkipsShallowLinesAndReadsSHA256(t *testing.T) {
	long := strings.Repeat("c", 64)
	body := []byte(pkt("shallow "+oidA+"\n") + string(pushBody(long+" "+long+" refs/heads/x")))
	updates, err := ParseReceivePackCommands(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Ref != "refs/heads/x" {
		t.Errorf("updates = %v", updates)
	}
}

// Every way a command list can fail to be one is refused, never read as
// "no commands": that is the difference between this failing closed and
// failing open.
func TestParseReceivePackCommandsRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"no flush":        pkt(oidA + " " + oidB + " refs/heads/main\n"),
		"bad length":      "zzzz",
		"short length":    "0003",
		"overlong length": "0fff" + oidA,
		"two fields":      pkt(oidA+" refs/heads/main\n") + "0000",
		"four fields":     pkt(oidA+" "+oidB+" refs/heads/main extra\n") + "0000",
		"bad oid":         pkt(strings.Repeat("g", 40)+" "+oidB+" refs/heads/main\n") + "0000",
		"empty ref":       pkt(oidA+" "+oidB+" \n") + "0000",
		"signed push":     pkt("push-cert\x00report-status\n") + pkt("certificate version 0.1\n") + "0000",
		"shallow after a command": pkt(oidA+" "+oidB+" refs/heads/x\n") +
			pkt("shallow "+oidA+"\n") + "0000",
	}
	for name, body := range cases {
		if updates, err := ParseReceivePackCommands([]byte(body)); err == nil {
			t.Errorf("%s: parsed as %v, want an error", name, updates)
		}
	}
}

func TestCommandListInflatesAGzipEncodedPush(t *testing.T) {
	raw := pushBody(oidA + " " + oidB + " refs/heads/main")
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw)
	_ = w.Close()
	data, err := commandList(bytes.NewReader(gz.Bytes()), "gzip")
	if err != nil {
		t.Fatal(err)
	}
	updates, err := ParseReceivePackCommands(data)
	if err != nil || len(updates) != 1 || updates[0].Ref != "refs/heads/main" {
		t.Fatalf("updates = %v, err = %v", updates, err)
	}
	if _, err := commandList(bytes.NewReader(raw), "br"); err == nil {
		t.Error("an encoding the proxy cannot read should be refused")
	}
}

func TestStaticPolicyAllowsOnlyItsPatterns(t *testing.T) {
	policy := testPolicy(t, StaticRepo{Repo: "github.com/acme/widgets", Access: AccessPush,
		Push: RefRules{{Pattern: "refs/heads/silo/*"}, {Pattern: "refs/heads/scratch", Delete: true}, {Pattern: "refs/tags/v*"}}})
	repo := Repo{Host: "github.com", Owner: "acme", Name: "widgets"}
	ctx := context.Background()
	// No fast-forward check: a create, a delete a pattern allows, and
	// nothing at all are allowed without one.
	for _, ok := range [][]RefUpdate{
		{{zero, oidB, "refs/heads/silo/x"}},
		{{oidA, zero, "refs/heads/scratch"}},
		{{zero, oidB, "refs/tags/v1"}},
		nil,
	} {
		if d, err := policy.RefUpdates(ctx, Caller{}, repo, ok, nil); err != nil || d.Refusal != "" {
			t.Errorf("%v: refused (%q, %v), want allowed", ok, d.Refusal, err)
		}
	}
	for _, refused := range [][]RefUpdate{
		{{oidA, oidB, "refs/heads/main"}},
		{{oidA, zero, "refs/heads/main"}},
		{{oidA, zero, "refs/heads/silo/x"}},   // a delete no pattern allows
		{{zero, oidB, "refs/tags/silo/x"}},    // a tag no pattern names
		{{oidA, oidB, "refs/tags/v1"}},        // moving a tag without force
		{{oidA, oidB, "refs/heads/silo/x"}},   // not shown a fast-forward
		{{oidA, oidB, "silo/x"}},              // not a full ref
		{{zero, oidB, "refs/heads/silo/x/y"}}, // * is one segment
		{{zero, oidB, "refs/heads/silo/x"}, {oidA, oidB, "refs/heads/main"}},
	} {
		d, err := policy.RefUpdates(ctx, Caller{}, repo, refused, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(d.Refusal, "refusing to update github.com/acme/widgets") ||
			!strings.Contains(d.Refusal, "refs/heads/silo/*") {
			t.Errorf("%v: refusal = %q, want one naming the patterns", refused, d.Refusal)
		}
	}
}

func TestStaticPolicyForceAllowsANonFastForward(t *testing.T) {
	repo := Repo{Host: "github.com", Owner: "acme", Name: "widgets"}
	update := []RefUpdate{{oidA, oidB, "refs/heads/silo/x"}}
	notShown := func(string, string) (bool, string) { return false, "" }
	for force, want := range map[bool]string{false: "could not show", true: ""} {
		policy := testPolicy(t, StaticRepo{Repo: repo.String(), Access: AccessPush,
			Push: RefRules{{Pattern: "refs/heads/silo/*"}}, Force: force})
		d, err := policy.RefUpdates(context.Background(), Caller{}, repo, update, notShown)
		if err != nil {
			t.Fatal(err)
		}
		if (want == "") != (d.Refusal == "") || !strings.Contains(d.Refusal, want) {
			t.Errorf("force %v: refusal %q, want %q", force, d.Refusal, want)
		}
		if force && (len(d.Grants) != 1 || d.Grants[0].Kind != "force" || d.Grants[0].Caller["pattern"] != "refs/heads/silo/*") {
			t.Errorf("force: grants = %+v", d.Grants)
		}
	}
}

func TestHandleForwardsAPushOnlyOfAllowedRefs(t *testing.T) {
	p, forwarder, audit := newTestProxy(t)
	headers := merge(gitHeaders, map[string]string{
		"Authorization": basicAuthHeader("tok0"),
		"Content-Type":  "application/x-git-receive-pack-request",
		"Accept":        "application/x-git-receive-pack-result",
	})
	push := func(body []byte) ProxyResponse {
		return p.Handle(context.Background(), "POST", "/owner/repo.git/git-receive-pack", "", headers, body)
	}

	if resp := push(pushBody(zero + " " + oidB + " refs/heads/feature/x")); resp.Status != 200 {
		t.Fatalf("pushing an allowed branch: %d %s", resp.Status, resp.Body)
	}
	if len(forwarder.Calls) != 1 {
		t.Fatalf("the allowed branch was not forwarded: %d calls", len(forwarder.Calls))
	}
	if refs := audit.Entries()[0].Refs; len(refs) != 1 || refs[0].Ref != "refs/heads/feature/x" || refs[0].Kind != "create" {
		t.Errorf("audit refs = %+v", refs)
	}

	// A refused ref is answered the way receive-pack answers one --
	// report-status naming it ng -- so git prints the reason.
	for name, body := range map[string][]byte{
		"default branch":        pushBody(oidA + " " + oidB + " refs/heads/main"),
		"default delete":        pushBody(oidA + " " + zero + " refs/heads/main"),
		"allowed delete":        pushBody(oidA + " " + zero + " refs/heads/feature/x"),
		"tag":                   pushBody(zero + " " + oidB + " refs/tags/v1"),
		"allowed + main":        pushBody(zero+" "+oidB+" refs/heads/feature/x", oidA+" "+oidB+" refs/heads/main"),
		"unproven fast-forward": pushBody(oidA + " " + oidB + " refs/heads/feature/x"),
	} {
		resp := push(body)
		if resp.Status != 200 || resp.Headers["Content-Type"] != "application/x-git-receive-pack-result" ||
			!strings.Contains(string(resp.Body), "\x01000eunpack ok\n") || !strings.Contains(string(resp.Body), "ng refs/") ||
			!strings.Contains(string(resp.Body), "\x02refusing") {
			t.Errorf("%s: %d %v %q, want a report-status refusal that says why", name, resp.Status, resp.Headers, resp.Body)
		}
	}
	// One whose commands cannot be read has no report to put them in.
	if resp := push([]byte("not a pkt-line")); resp.Status != 403 || !strings.Contains(string(resp.Body), "refusing") {
		t.Errorf("unreadable: %d %q, want a 403 that says why", resp.Status, resp.Body)
	}
	if len(forwarder.Calls) != 1 {
		t.Errorf("a refused push reached the upstream: %d calls", len(forwarder.Calls))
	}
	entries := audit.Entries()
	last := entries[len(entries)-1]
	if !strings.HasPrefix(last.Outcome, "denied:") {
		t.Errorf("audit outcome = %q, want a denial", last.Outcome)
	}
}

func TestRejectPushAnswersInTheShapeTheClientAskedFor(t *testing.T) {
	updates := []RefUpdate{{Old: oidA, New: oidB, Ref: "refs/heads/main"}}
	if _, ok := rejectPush(updates, []string{"side-band-64k", "agent=git/2.43.0"}, "no"); ok {
		t.Error("a client that did not ask for report-status was sent one")
	}
	resp, ok := rejectPush(updates, []string{"report-status"}, "refusing\nmain")
	if !ok {
		t.Fatal("report-status without side-band was not answered with a report")
	}
	if want := pkt("unpack ok\n") + pkt("ng refs/heads/main refusing main\n") + "0000"; string(resp.Body) != want {
		t.Errorf("body = %q, want %q", resp.Body, want)
	}
	resp, _ = rejectPush(updates, []string{"report-status-v2", "side-band-64k"}, strings.Repeat("x", 2000))
	for rest := string(resp.Body); rest != "0000"; {
		n, err := strconv.ParseUint(rest[:4], 16, 16)
		if err != nil || n <= 5 || n > 1000 || (rest[4] != 1 && rest[4] != 2) {
			t.Fatalf("not a side-band packet at %q", rest)
		}
		rest = rest[n:]
	}
}
