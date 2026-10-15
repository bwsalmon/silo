package sandbox

import (
	"context"
	"errors"
	"fmt"
)

// Kinds is a Backend made of others: Create goes to K8s for a spec
// with a K8s target, else to the backend for the spec's kind, or
// Default; Find and Sessions ask every one of
// them. Networks are Default's, and the other backends' sandboxes join
// them by name, so every backend in it has to share Default's
// container runtime (granule runs its VMs on the local Docker).
type Kinds struct {
	Default Backend
	ByKind  map[string]Backend
	// K8s runs the sandboxes whose backend is a cluster (journey 9).
	K8s Backend
}

var (
	_ Backend       = (*Kinds)(nil)
	_ SessionLister = (*Kinds)(nil)
	_ Networker     = (*Kinds)(nil)
)

func (k *Kinds) all() []Backend {
	var out []Backend
	if k.Default != nil {
		out = append(out, k.Default)
	}
	for _, b := range k.ByKind {
		if b != nil && b != k.Default {
			out = append(out, b)
		}
	}
	if k.K8s != nil {
		out = append(out, k.K8s)
	}
	return out
}

// Create implements Backend.
func (k *Kinds) Create(ctx context.Context, spec SandboxSpec) (Sandbox, error) {
	b, ok := k.ByKind[spec.Kind]
	if !ok {
		b = k.Default
	}
	if spec.K8s != nil {
		b = k.K8s
	}
	if b == nil {
		return nil, fmt.Errorf("sandbox %q: no backend runs kind %q", spec.Name, spec.Kind)
	}
	return b.Create(ctx, spec)
}

// Find implements Backend: every backend's sandboxes of session.
func (k *Kinds) Find(ctx context.Context, session string) ([]Sandbox, error) {
	var found []Sandbox
	var errs []error
	for _, b := range k.all() {
		f, err := b.Find(ctx, session)
		found = append(found, f...)
		errs = append(errs, err)
	}
	return found, errors.Join(errs...)
}

// Sessions implements SessionLister: the sessions of every backend
// that lists them.
func (k *Kinds) Sessions(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var sessions []string
	var errs []error
	for _, b := range k.all() {
		l, ok := b.(SessionLister)
		if !ok {
			continue
		}
		s, err := l.Sessions(ctx)
		errs = append(errs, err)
		for _, id := range s {
			if !seen[id] {
				seen[id] = true
				sessions = append(sessions, id)
			}
		}
	}
	return sessions, errors.Join(errs...)
}

func (k *Kinds) networker() (Networker, error) {
	if k.Default == nil {
		return nil, errors.New("no local sandbox backend makes networks")
	}
	if nw, ok := k.Default.(Networker); ok {
		return nw, nil
	}
	return nil, errors.New("the sandbox backend cannot make networks")
}

// CreateNetwork implements Networker with Default's.
func (k *Kinds) CreateNetwork(ctx context.Context, session string) (Network, error) {
	nw, err := k.networker()
	if err != nil {
		return Network{}, err
	}
	return nw.CreateNetwork(ctx, session)
}

// RemoveNetwork implements Networker with Default's.
func (k *Kinds) RemoveNetwork(ctx context.Context, name string) error {
	nw, err := k.networker()
	if err != nil {
		return err
	}
	return nw.RemoveNetwork(ctx, name)
}
