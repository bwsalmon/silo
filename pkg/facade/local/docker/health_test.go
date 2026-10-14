package docker

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
)

// probes is a fake ExecFunc for healthchecks: a probe of "/bin/sh -c
// ok" passes, and any other fails with what it ran on its stderr. It
// counts each container's probes.
type probes struct {
	mu    sync.Mutex
	runs  map[string]int
	procs []runtime.Process
}

func newProbes(e *env) *probes {
	p := &probes{runs: map[string]int{}}
	f := func(id string, proc runtime.Process, stdio runtime.ExecIO) int {
		p.mu.Lock()
		p.runs[id]++
		p.procs = append(p.procs, proc)
		p.mu.Unlock()
		if strings.Join(proc.Args, " ") == "/bin/sh -c ok" {
			fmt.Fprint(stdio.Stdout, "fine")
			return 0
		}
		fmt.Fprintf(stdio.Stderr, "%s: no", strings.Join(proc.Args, " "))
		return 1
	}
	e.vm.ExecFunc, e.sbx.ExecFunc = f, f
	return p
}

func (p *probes) count(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[id]
}

// hc is a create body with a healthcheck of test, every 10ms, and a
// tmpfs over the image's volume, so that a silo's volume quota doesn't
// bound how many a test makes.
func hc(test ...string) obj {
	return obj{"Healthcheck": obj{"Test": test, "Interval": int64(10 * time.Millisecond), "Retries": 2},
		"HostConfig": obj{"Tmpfs": obj{"/data": ""}}}
}

// healthOf is a container's State.Health, or nil.
func (e *env) healthOf(id string) obj {
	e.t.Helper()
	h, _ := e.ok(e.a, "GET", "/v1.47/containers/"+id+"/json", nil)["State"].(obj)["Health"].(obj)
	return h
}

// waitHealth waits for a container's health status to be want.
func (e *env) waitHealth(id, want string) obj {
	e.t.Helper()
	var h obj
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if h = e.healthOf(id); h != nil && h["Status"] == want {
			return h
		}
	}
	e.t.Fatalf("%s: health %v, want %s", id[:12], h, want)
	return nil
}

// still checks that id isn't probed for a while.
func still(t *testing.T, p *probes, id string) {
	t.Helper()
	n := p.count(id)
	time.Sleep(60 * time.Millisecond)
	if m := p.count(id); m != n {
		t.Errorf("%s: probed %d more times after its probes should have stopped", id[:12], m-n)
	}
}

func TestHealthcheck(t *testing.T) {
	e := newEnv(t)
	p := newProbes(e)
	good := e.create(e.a, "good", hc("CMD-SHELL", "ok"))
	bad := e.create(e.a, "bad", hc("CMD", "false", "x"))
	plain := e.create(e.a, "plain", obj{"HostConfig": obj{"Tmpfs": obj{"/data": ""}}})
	if e.healthOf(good) != nil {
		t.Error("a container that hasn't started has health")
	}
	if got := e.ok(e.a, "GET", "/v1.47/containers/good/json", nil)["Config"].(obj)["Healthcheck"].(obj)["Test"]; fmt.Sprint(got) != "[CMD-SHELL ok]" {
		t.Errorf("Config.Healthcheck.Test %v", got)
	}
	for _, id := range []string{good, bad, plain} {
		e.ok(e.a, "POST", "/v1.47/containers/"+id+"/start", nil)
	}
	if h := e.healthOf(good); h == nil || (h["Status"] != healthStarting && h["Status"] != healthHealthy) {
		t.Errorf("health at start: %v, want starting", h)
	}
	if e.healthOf(plain) != nil {
		t.Error("a container without a HEALTHCHECK has health")
	}
	h := e.waitHealth(good, healthHealthy)
	if l := h["Log"].([]any); len(l) == 0 || l[0].(obj)["Output"] != "fine" || l[0].(obj)["ExitCode"] != 0.0 {
		t.Errorf("healthy log: %v", l)
	}
	h = e.waitHealth(bad, healthUnhealthy)
	if h["FailingStreak"].(float64) < 2 {
		t.Errorf("unhealthy with a failing streak of %v, want at least Retries", h["FailingStreak"])
	}
	l := h["Log"].([]any)
	if l[0].(obj)["ExitCode"] != 1.0 || l[0].(obj)["Output"] != "false x: no" {
		t.Errorf("unhealthy log: %v", l)
	}
	// The probe runs as the container's process does.
	p.mu.Lock()
	if pr := p.procs[0]; !strings.Contains(strings.Join(pr.Env, " "), "A=1") {
		t.Errorf("probe env %v, want the container's", pr.Env)
	}
	p.mu.Unlock()
	// Docker keeps the last five results.
	time.Sleep(100 * time.Millisecond)
	if l := e.healthOf(bad)["Log"].([]any); len(l) != healthLogKeep {
		t.Errorf("%d results kept, want %d", len(l), healthLogKeep)
	}

	// ps shows health in Status, and filters on it.
	for _, c := range e.list(e.a, "GET", "/v1.47/containers/json") {
		c := c.(obj)
		want := map[string]string{"/good": "(healthy)", "/bad": "(unhealthy)", "/plain": "Up"}[c["Names"].([]any)[0].(string)]
		if !strings.Contains(c["Status"].(string), want) {
			t.Errorf("%v: Status %q, want %q in it", c["Names"], c["Status"], want)
		}
	}
	for filter, want := range map[string]string{"healthy": "/good", "unhealthy": "/bad", "none": "/plain"} {
		got := e.list(e.a, "GET", `/v1.47/containers/json?filters={"health":["`+filter+`"]}`)
		if len(got) != 1 || got[0].(obj)["Names"].([]any)[0] != want {
			t.Errorf("health=%s: %v, want %s", filter, got, want)
		}
	}

	// Stop, kill, an exit and a remove each end the probes; a start
	// starts them again, from starting.
	e.ok(e.a, "POST", "/v1.47/containers/good/stop?t=0", nil)
	still(t, p, good)
	if h := e.healthOf(good); h == nil || h["Status"] != healthHealthy {
		t.Errorf("a stopped container's health %v, want its last", h)
	}
	e.ok(e.a, "POST", "/v1.47/containers/good/start", nil)
	e.waitHealth(good, healthHealthy)
	e.ok(e.a, "POST", "/v1.47/containers/good/kill", nil)
	still(t, p, good)
	e.ok(e.a, "POST", "/v1.47/containers/good/restart?t=0", nil)
	e.waitHealth(good, healthHealthy)
	if err := e.vm.Exit(good, 3); err != nil {
		t.Fatal(err)
	}
	still(t, p, good)
	e.ok(e.a, "DELETE", "/v1.47/containers/bad?force=1", nil)
	still(t, p, bad)
	e.facade.healthMu.Lock()
	_, kept := e.facade.health[bad]
	e.facade.healthMu.Unlock()
	if kept {
		t.Error("a removed container's health is kept")
	}
}

// A class whose exec doesn't enter the container is unhealthy at once,
// saying why, and never probed.
func TestHealthcheckNoExec(t *testing.T) {
	e := newEnv(t)
	p := newProbes(e)
	e.vm.NoExec = "no exec here (plan item 9)"
	id := e.create(e.a, "vm", hc("CMD-SHELL", "ok"))
	e.ok(e.a, "POST", "/v1.47/containers/vm/start", nil)
	h := e.healthOf(id)
	if h == nil || h["Status"] != healthUnhealthy || h["Log"].([]any)[0].(obj)["Output"] != e.vm.NoExec {
		t.Fatalf("health %v, want unhealthy saying why", h)
	}
	still(t, p, id)
	if p.count(id) != 0 {
		t.Errorf("probed %d times", p.count(id))
	}
	if h := e.healthOf(id); h["Status"] != healthUnhealthy {
		t.Errorf("health %v, want it to stay unhealthy", h)
	}
	// The sandbox class's exec works.
	body := hc("CMD-SHELL", "ok")
	body["HostConfig"].(obj)["Runtime"] = RuntimeSandbox
	sbx := e.create(e.a, "sbx", body)
	e.ok(e.a, "POST", "/v1.47/containers/sbx/start", nil)
	e.waitHealth(sbx, healthHealthy)
}

// A create's healthcheck is merged over its image's; NONE turns the
// image's off.
func TestHealthcheckImage(t *testing.T) {
	e := newEnv(t)
	p := newProbes(e)
	e.images.healthcheck = &HealthConfig{Test: []string{"CMD-SHELL", "ok"}, Interval: time.Hour, Retries: 1}
	inherit := e.create(e.a, "inherit", obj{"Healthcheck": obj{"Interval": int64(10 * time.Millisecond)}})
	none := e.create(e.a, "none", obj{"Healthcheck": obj{"Test": []string{"NONE"}}, "HostConfig": obj{"Tmpfs": obj{"/data": ""}}})
	for _, id := range []string{inherit, none} {
		e.ok(e.a, "POST", "/v1.47/containers/"+id+"/start", nil)
	}
	e.waitHealth(inherit, healthHealthy)
	got := e.ok(e.a, "GET", "/v1.47/containers/inherit/json", nil)["Config"].(obj)["Healthcheck"].(obj)
	if fmt.Sprint(got["Test"]) != "[CMD-SHELL ok]" || got["Interval"] != float64(10*time.Millisecond) || got["Retries"] != 1.0 {
		t.Errorf("merged healthcheck %v", got)
	}
	if e.healthOf(none) != nil {
		t.Error("a container with a HEALTHCHECK of NONE has health")
	}
	still(t, p, none)
	if p.count(none) != 0 {
		t.Errorf("NONE probed %d times", p.count(none))
	}
	img := e.ok(e.a, "GET", "/v1.47/images/alpine/json", nil)["Config"].(obj)["Healthcheck"].(obj)
	if fmt.Sprint(img["Test"]) != "[CMD-SHELL ok]" {
		t.Errorf("image inspect's Healthcheck %v", img)
	}
}

func TestHealthcheckRefused(t *testing.T) {
	e := newEnv(t)
	e.pull(e.a, "alpine")
	for _, c := range []struct {
		hc   obj
		want string
	}{
		{obj{"Test": []string{"RUN", "x"}}, "must start with NONE, CMD or CMD-SHELL"},
		{obj{"Test": []string{"CMD"}}, "names no command"},
		{obj{"Test": []string{"CMD", "x"}, "Interval": 5}, "Interval in Healthcheck cannot be less than 1ms"},
		{obj{"Test": []string{"CMD", "x"}, "Retries": -1}, "Retries in Healthcheck cannot be negative"},
	} {
		e.fail(400, c.want, e.a, "POST", "/v1.47/containers/create", obj{"Image": "alpine", "Healthcheck": c.hc})
	}
}

// After a restart, silod watches its running containers again and
// probes their health from starting.
func TestHealthcheckAdopt(t *testing.T) {
	e := newEnv(t)
	p := newProbes(e)
	id := e.create(e.a, "web", hc("CMD-SHELL", "ok"))
	e.ok(e.a, "POST", "/v1.47/containers/web/start", nil)
	e.waitHealth(id, healthHealthy)
	e.create(e.a, "stopped", hc("CMD-SHELL", "ok"))
	e.facade.Close()
	still(t, p, id)

	again := New(e.db, Options{Drivers: []runtime.Driver{e.vm, e.sbx}, Storage: &storage.Dir{Root: e.vols}})
	t.Cleanup(again.Close)
	if err := again.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if st := again.healthStatus(id); st != healthStarting && st != healthHealthy {
		t.Errorf("adopted health %q, want starting", st)
	}
	n := p.count(id)
	deadline := time.Now().Add(5 * time.Second)
	for again.healthStatus(id) != healthHealthy && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if again.healthStatus(id) != healthHealthy || p.count(id) == n {
		t.Fatalf("adopted health %q after %d probes", again.healthStatus(id), p.count(id)-n)
	}
	if len(again.health) != 1 {
		t.Errorf("adopted %d containers' health, want only the running one's", len(again.health))
	}
	// Its exit is settled, which ends the probes.
	if err := e.vm.Exit(id, 0); err != nil {
		t.Fatal(err)
	}
	still(t, p, id)
}

// Failures in the start period don't count while the container is
// starting; after it, Retries of them in a row are unhealthy.
func TestHealthRecord(t *testing.T) {
	s := &Server{health: map[string]*health{}}
	c := &Container{ID: "c", Healthcheck: &HealthConfig{Test: []string{"CMD", "x"}, Retries: 2}}
	run := &healthRun{}
	s.health["c"] = &health{Status: healthStarting, run: run}
	fail, pass := healthResult{ExitCode: 1}, healthResult{}
	steps := []struct {
		r       healthResult
		inStart bool
		status  string
		streak  int
	}{
		{fail, true, healthStarting, 0},
		{fail, true, healthStarting, 0},
		{pass, true, healthHealthy, 0},
		{fail, true, healthHealthy, 1},
		{fail, false, healthUnhealthy, 2},
		{pass, false, healthHealthy, 0},
	}
	for i, st := range steps {
		s.record(c, run, st.r, st.inStart)
		h := s.health["c"]
		if h.Status != st.status || h.FailingStreak != st.streak {
			t.Errorf("step %d: %s, streak %d; want %s, %d", i, h.Status, h.FailingStreak, st.status, st.streak)
		}
	}
	// A run that has been stopped records nothing.
	s.record(c, &healthRun{}, fail, false)
	if s.health["c"].FailingStreak != 0 {
		t.Error("a stale run's result was recorded")
	}
}
