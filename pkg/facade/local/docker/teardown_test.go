package docker

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// listeners records what the facade asks of the gateway's listeners.
type listeners struct {
	mu     sync.Mutex
	calls  []string
	refuse bool
}

func (l *listeners) Open(_ context.Context, ps []Published) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refuse {
		return fmt.Errorf("Bind for 127.0.0.1:%d failed: %w", ps[0].Port, ErrPortTaken)
	}
	for _, p := range ps {
		l.calls = append(l.calls, fmt.Sprintf("open %s %d->%s host=%v", p.Silo, p.Port, p.Target, p.Host))
	}
	return nil
}

func (l *listeners) Close(container string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "close "+container)
}

func (l *listeners) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.calls
	l.calls = nil
	return c
}

// TestListeners: a start opens its container's published ports, with
// Host where the silo's ingress is host; a stop and a remove close
// them; and a host port that is taken refuses the start with 409.
func TestListeners(t *testing.T) {
	e := newEnv(t)
	l := &listeners{}
	e.facade.opts.Listeners = l
	id := e.create(e.root, "web", obj{"HostConfig": obj{"PortBindings": obj{"80/tcp": []obj{{"HostPort": "8080"}}}}})
	e.ok(e.root, "POST", "/v1.47/containers/web/start", nil)
	got := l.take()
	if len(got) != 1 || !regexpMatch(`^open `+e.root+` 8080->10\.\d+\.\d+\.\d+:80 host=true$`, got[0]) {
		t.Errorf("start: %v", got)
	}
	e.ok(e.root, "POST", "/v1.47/containers/web/stop?t=0", nil)
	if got := l.take(); !slices.Contains(got, "close "+id) {
		t.Errorf("stop: %v", got)
	}
	// A child's ingress is none: no host listener.
	e.create(e.a, "web", obj{"HostConfig": obj{"PortBindings": obj{"80/tcp": []obj{{"HostPort": "8080"}}}}})
	e.ok(e.a, "POST", "/v1.47/containers/web/start", nil)
	// The stopped container's watch settles it in the background, and
	// closes its listeners again whenever it gets there.
	got = slices.DeleteFunc(l.take(), func(c string) bool { return c == "close "+id })
	if len(got) != 1 || !regexpMatch(`host=false$`, got[0]) {
		t.Errorf("child's start: %v", got)
	}
	l.refuse = true
	e.fail(409, "port is already allocated", e.root, "POST", "/v1.47/containers/web/start", nil)
	if got := e.used(e.root, ClassMemoryMB); got != 0 {
		t.Errorf("memory_mb after a refused start: %d", got)
	}
	if c := e.facade.state(ctx, &Container{ID: id, Class: runtime.VM}); c.State == runtime.Running {
		t.Errorf("a start refused for its port ran the container: %+v", c)
	}
}

// TestDeleteSilo: a silo's delete removes its containers', networks',
// volumes' and images' rows, and gives back their charges.
func TestDeleteSilo(t *testing.T) {
	e := newEnv(t)
	e.ok(e.a, "POST", "/v1.47/networks/create", obj{"Name": "back"})
	e.ok(e.a, "POST", "/v1.47/volumes/create", obj{"Name": "data"})
	l := &listeners{}
	e.facade.opts.Listeners = l
	var unwired []string
	e.facade.opts.Unwire = func(_ context.Context, c *Container) error {
		unwired = append(unwired, c.ID)
		return nil
	}
	id := e.create(e.a, "web", obj{"HostConfig": obj{"NetworkMode": "back"}})
	for _, c := range []string{ClassNetworks, ClassVolumes, ClassContainers, ClassDiskGB} {
		if e.used(e.a, c) == 0 {
			t.Fatalf("%s not charged before the delete", c)
		}
	}
	// The containers are the runtime's to delete first
	// (local.Containers.DeleteContents).
	if err := e.vm.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.facade.DeleteSilo(ctx, e.a); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{ClassNetworks, ClassVolumes, ClassContainers, ClassDiskGB} {
		if got := e.used(e.a, c); got != 0 {
			t.Errorf("%s after the delete: %d", c, got)
		}
	}
	for _, p := range []string{"/v1.47/containers/json?all=1", "/v1.47/networks", "/v1.47/volumes", "/v1.47/images/json"} {
		code, b, _ := e.raw(e.a, "GET", p, nil, nil)
		if code != 200 || (string(b) != "[]\n" && string(b) != "[]" && !regexpMatch(`"Volumes":\[\]`, string(b))) {
			t.Errorf("%s after the delete: %d %s", p, code, b)
		}
	}
	if !slices.Contains(unwired, id) || !slices.Contains(l.take(), "close "+id) {
		t.Errorf("the container's wiring and listeners were not undone: %v", unwired)
	}
	// Again, for a delete that failed half way.
	if err := e.facade.DeleteSilo(ctx, e.a); err != nil {
		t.Errorf("a second delete: %v", err)
	}
}

func regexpMatch(re, s string) bool { return regexp.MustCompile(re).MatchString(s) }
