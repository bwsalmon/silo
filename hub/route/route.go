// Package route is an http.ServeMux that remembers the patterns it was
// given, so that the hub's routes can be listed from the router itself
// rather than from a list kept beside it: the security suite
// (hub/security, docs/security-tests.md) asserts an authorization
// outcome for every route the hub serves, and fails on one it was not
// told about.
package route

import (
	"net/http"
	"slices"
)

// Lister is a handler that can say which patterns it serves.
type Lister interface {
	Patterns() []string
}

// Mux is an http.ServeMux that records its patterns. A pattern whose
// handler is itself a Lister (a sub-router mounted under a prefix)
// stands for that handler's patterns instead.
type Mux struct {
	http.ServeMux
	entries []entry
}

type entry struct {
	pattern string
	h       http.Handler
}

// NewMux returns an empty Mux.
func NewMux() *Mux { return &Mux{} }

// Handle registers h for pattern, as http.ServeMux.Handle does.
func (m *Mux) Handle(pattern string, h http.Handler) {
	m.entries = append(m.entries, entry{pattern, h})
	m.ServeMux.Handle(pattern, h)
}

// HandleFunc registers f for pattern, as http.ServeMux.HandleFunc does.
func (m *Mux) HandleFunc(pattern string, f func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(f))
}

// Patterns lists every pattern m serves, sorted and without repeats:
// its own, and in place of a mounted Lister's mount, the Lister's.
func (m *Mux) Patterns() []string {
	var out []string
	for _, e := range m.entries {
		if l, ok := e.h.(Lister); ok {
			out = append(out, l.Patterns()...)
			continue
		}
		out = append(out, e.pattern)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Wrap is h, a wrapper around the routes l serves (a same-origin
// check, a metric), still listing l's patterns.
func Wrap(l Lister, h http.Handler) http.Handler {
	return wrapped{h, l}
}

type wrapped struct {
	http.Handler
	l Lister
}

func (w wrapped) Patterns() []string { return w.l.Patterns() }
