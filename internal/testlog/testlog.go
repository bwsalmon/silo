// Package testlog gives servers under test a logger that is safe to call
// after the test has returned.
package testlog

import (
	"sync"
	"testing"
)

// Logf is t.Logf until the test's cleanups run, and a no-op after.
// httptest.Server.Close does not wait for hijacked (WebSocket)
// connections, so a goroutine serving one can still log once its test
// has returned, which the testing package reports as a race (or a panic).
func Logf(t testing.TB) func(string, ...any) {
	var (
		mu   sync.Mutex
		done bool
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		done = true
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}
