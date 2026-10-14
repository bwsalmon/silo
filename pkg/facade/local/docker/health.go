package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// HealthConfig is a container's HEALTHCHECK, as Docker's
// Config.Healthcheck and an image config's Healthcheck say it.
// Durations are nanoseconds, as Docker's are; zero is the default.
type HealthConfig struct {
	// Test is ["CMD", args...], ["CMD-SHELL", script] or ["NONE"];
	// empty inherits the image's.
	Test          []string      `json:",omitempty"`
	Interval      time.Duration `json:",omitempty"`
	Timeout       time.Duration `json:",omitempty"`
	StartPeriod   time.Duration `json:",omitempty"`
	StartInterval time.Duration `json:",omitempty"`
	Retries       int           `json:",omitempty"`
}

// Docker's defaults for a HEALTHCHECK's zero fields, and what a
// container's health keeps of its probes.
const (
	healthInterval      = 30 * time.Second
	healthTimeout       = 30 * time.Second
	healthStartInterval = 5 * time.Second
	healthRetries       = 3
	healthLogKeep       = 5
	healthOutputMax     = 4096
)

// Health statuses, as Docker reports them in State.Health.Status.
const (
	healthStarting  = "starting"
	healthHealthy   = "healthy"
	healthUnhealthy = "unhealthy"
)

// mergeHealth is the create's healthcheck over the image's, as
// Docker merges them: no create healthcheck is the image's, and each
// of its zero fields is the image's.
func mergeHealth(create, image *HealthConfig) *HealthConfig {
	if create == nil {
		if image == nil {
			return nil
		}
		out := *image
		return &out
	}
	out := *create
	if image == nil {
		return &out
	}
	if len(out.Test) == 0 {
		out.Test = image.Test
	}
	if out.Interval == 0 {
		out.Interval = image.Interval
	}
	if out.Timeout == 0 {
		out.Timeout = image.Timeout
	}
	if out.StartPeriod == 0 {
		out.StartPeriod = image.StartPeriod
	}
	if out.StartInterval == 0 {
		out.StartInterval = image.StartInterval
	}
	if out.Retries == 0 {
		out.Retries = image.Retries
	}
	return &out
}

// checkHealth refuses a healthcheck Docker's create refuses.
func checkHealth(h *HealthConfig) error {
	if h == nil {
		return nil
	}
	if len(h.Test) > 0 {
		switch h.Test[0] {
		case "NONE", "CMD", "CMD-SHELL":
		default:
			return invalid("silo: Healthcheck.Test must start with NONE, CMD or CMD-SHELL, not %q", h.Test[0])
		}
		if h.Test[0] != "NONE" && len(h.Test) < 2 {
			return invalid("silo: Healthcheck.Test %s names no command", h.Test[0])
		}
	}
	for name, d := range map[string]time.Duration{"Interval": h.Interval, "Timeout": h.Timeout,
		"StartPeriod": h.StartPeriod, "StartInterval": h.StartInterval} {
		if d != 0 && d < time.Millisecond {
			return invalid("%s in Healthcheck cannot be less than 1ms", name)
		}
	}
	if h.Retries < 0 {
		return invalid("Retries in Healthcheck cannot be negative")
	}
	return nil
}

// probes is whether h is a healthcheck to run: one with a command,
// not NONE.
func (h *HealthConfig) probes() bool {
	return h != nil && len(h.Test) > 1 && h.Test[0] != "NONE"
}

// args are the probe's process arguments.
func (h *HealthConfig) args() []string {
	if h.Test[0] == "CMD-SHELL" {
		return []string{"/bin/sh", "-c", h.Test[1]}
	}
	return slices.Clone(h.Test[1:])
}

func orDefault(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// health is a container's health while silod runs: its probes' status
// and their last results. It is not stored: after a restart a running
// container's health starts again from starting (Adopt).
type health struct {
	Status        string
	FailingStreak int
	Log           []healthResult
	// run is the probe loop's, which stop ends; a loop whose run is no
	// longer this one's records nothing.
	run *healthRun
}

type healthRun struct{ cancel context.CancelFunc }

// healthResult is one probe's, as State.Health.Log shows it.
type healthResult struct {
	Start, End time.Time
	ExitCode   int
	Output     string
}

// startHealth starts c's probes, with its health starting, if it has a
// HEALTHCHECK. A class with no working exec (runtime.ExecGapper) is
// reported unhealthy at once, saying why, and never probed.
func (s *Server) startHealth(c *Container) {
	if !c.Healthcheck.probes() {
		return
	}
	s.stopHealth(c.ID)
	d := s.drivers[c.Class]
	h := &health{Status: healthStarting, Log: []healthResult{}}
	if g, ok := d.(runtime.ExecGapper); ok && g.ExecGap() != "" {
		now := time.Now().UTC()
		h.Status, h.FailingStreak = healthUnhealthy, 1
		h.Log = append(h.Log, healthResult{Start: now, End: now, ExitCode: -1, Output: g.ExecGap()})
		s.healthMu.Lock()
		s.health[c.ID] = h
		s.healthMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.bg)
	h.run = &healthRun{cancel: cancel}
	s.healthMu.Lock()
	s.health[c.ID] = h
	s.healthMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.probeLoop(ctx, d, c, h.run)
	}()
}

// stopHealth ends id's probes, keeping its last health, as Docker's
// inspect of a stopped container shows it.
func (s *Server) stopHealth(id string) {
	s.stopHealthRun(id, nil)
}

// stopHealthRun ends id's probes if they are run's (any, if run is
// nil): the watcher of one start doesn't end the next start's.
func (s *Server) stopHealthRun(id string, run *healthRun) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if h := s.health[id]; h != nil && h.run != nil && (run == nil || h.run == run) {
		h.run.cancel()
		h.run = nil
	}
}

// healthRunOf is id's probe run, or nil if it has none.
func (s *Server) healthRunOf(id string) *healthRun {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if h := s.health[id]; h != nil {
		return h.run
	}
	return nil
}

// forgetHealth ends id's probes and forgets its health.
func (s *Server) forgetHealth(id string) {
	s.stopHealth(id)
	s.healthMu.Lock()
	delete(s.health, id)
	s.healthMu.Unlock()
}

// probeLoop probes c every interval (StartInterval while it is in its
// start period and not yet healthy) until ctx ends.
func (s *Server) probeLoop(ctx context.Context, d runtime.Driver, c *Container, run *healthRun) {
	hc := c.Healthcheck
	started := time.Now()
	for {
		wait := orDefault(hc.Interval, healthInterval)
		if hc.StartPeriod > 0 && time.Since(started) < hc.StartPeriod && s.healthStatus(c.ID) == healthStarting {
			wait = orDefault(hc.StartInterval, healthStartInterval)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		r, ok := probe(ctx, d, c)
		if !ok {
			continue
		}
		s.record(c, run, r, time.Since(started) < hc.StartPeriod)
	}
}

// probe runs c's healthcheck once, through its driver's exec, with the
// container's environment, user and working directory. A probe that
// outlives its Timeout ends -1, as Docker's does. ok is false for a
// probe that says nothing of the container's health: one stopped with
// the loop, or refused because the container isn't running (paused,
// or stopping).
func probe(ctx context.Context, d runtime.Driver, c *Container) (r healthResult, ok bool) {
	hc := c.Healthcheck
	timeout := orDefault(hc.Timeout, healthTimeout)
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	w := &probeOutput{}
	r.Start = time.Now().UTC()
	code, err := d.Exec(pctx, c.ID, runtime.Process{Args: hc.args(), Env: c.Process.Env, User: c.Process.User,
		Workdir: c.Process.Workdir}, runtime.ExecIO{Stdout: w, Stderr: w})
	r.End = time.Now().UTC()
	switch {
	case ctx.Err() != nil, errors.Is(err, runtime.ErrState), errors.Is(err, runtime.ErrNotFound):
		return r, false
	case pctx.Err() != nil:
		r.ExitCode, r.Output = -1, fmt.Sprintf("Health check exceeded timeout (%v)", timeout)
	case err != nil:
		r.ExitCode, r.Output = -1, err.Error()
	default:
		r.ExitCode, r.Output = code, w.String()
	}
	return r, true
}

// record applies a probe's result to c's health, as Docker does: a
// pass is healthy; a failure adds to the failing streak, and Retries
// of them in a row are unhealthy. A failure in the start period, while
// the container hasn't yet been healthy, doesn't count.
func (s *Server) record(c *Container, run *healthRun, r healthResult, inStart bool) {
	retries := c.Healthcheck.Retries
	if retries == 0 {
		retries = healthRetries
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h := s.health[c.ID]
	if h == nil || h.run != run {
		return
	}
	h.Log = append(h.Log, r)
	if len(h.Log) > healthLogKeep {
		h.Log = h.Log[len(h.Log)-healthLogKeep:]
	}
	switch {
	case r.ExitCode == 0:
		h.Status, h.FailingStreak = healthHealthy, 0
	case inStart && h.Status == healthStarting:
	default:
		h.FailingStreak++
		if h.FailingStreak >= retries {
			h.Status = healthUnhealthy
		}
	}
}

// healthStatus is id's health status, or "" if it has none.
func (s *Server) healthStatus(id string) string {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if h := s.health[id]; h != nil {
		return h.Status
	}
	return ""
}

// healthJSON is id's State.Health, or nil if it has none.
func (s *Server) healthJSON(id string) map[string]any {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h := s.health[id]
	if h == nil {
		return nil
	}
	log := []map[string]any{}
	for _, r := range h.Log {
		log = append(log, map[string]any{"Start": r.Start.Format(time.RFC3339Nano), "End": r.End.Format(time.RFC3339Nano),
			"ExitCode": r.ExitCode, "Output": r.Output})
	}
	return map[string]any{"Status": h.Status, "FailingStreak": h.FailingStreak, "Log": log}
}

// probeOutput is a probe's stdout and stderr together, as Docker keeps
// them: the first healthOutputMax bytes. Both streams may write at
// once.
type probeOutput struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *probeOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := healthOutputMax - w.b.Len(); room > 0 {
		w.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (w *probeOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
