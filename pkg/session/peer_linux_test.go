package session_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/session"
)

func TestInCgroup(t *testing.T) {
	const scope = "silo-agent-a1.scope"
	for _, c := range []struct {
		path, manager string
		in            bool
	}{
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/silo-agent-a1.scope", "user@1000.service", true},
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/silo-agent-a1.scope", "", true},
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/silo-agent-b2.scope", "user@1000.service", false},
		{"/user.slice/user-1001.slice/user@1001.service/app.slice/silo-agent-a1.scope", "user@1000.service", false},
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/silo-agent-a1.scope/sub", "user@1000.service", false},
		{"/", "", false},
	} {
		if got := session.InCgroup(c.path, scope, c.manager); got != c.in {
			t.Errorf("InCgroup(%s, %s) = %v", c.path, c.manager, got)
		}
	}
	if m := session.UserManager("/user.slice/user-1000.slice/user@1000.service/silo.slice"); m != "user@1000.service" {
		t.Errorf("UserManager: %q", m)
	}
	if m := session.UserManager("/system.slice/silod.service"); m != "" {
		t.Errorf("UserManager of a system service: %q", m)
	}
}

// A session socket with ScopePeer serves a process in the session's
// cgroup, here the test's own, and closes any other's connection
// unserved.
func TestScopePeer(t *testing.T) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip("no /proc/self/cgroup")
	}
	var own string
	for _, l := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			own = p[strings.LastIndex(p, "/")+1:]
		}
	}
	if own == "" {
		t.Skip("not in a named cgroup v2 group")
	}
	get := func(socket string) error {
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}}}
		resp, err := c.Get("http://silo/nowhere")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("served: %s", resp.Status)
		}
		return nil
	}
	scopes := map[string]string{"in": own, "out": "silo-agent-out.scope"}
	s := &session.Sockets{Dir: t.TempDir(), Peer: session.ScopePeer(func(agent string) string { return scopes[agent] })}
	defer s.CloseAll()
	in, err := s.Open("in", "silo1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Open("out", "silo1")
	if err != nil {
		t.Fatal(err)
	}
	if err := get(in); err != nil {
		t.Errorf("a peer in the session's cgroup was refused: %v", err)
	}
	if err := get(out); err == nil {
		t.Error("a peer outside the session's scope was served")
	}
}
