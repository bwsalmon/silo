//go:build !linux

package router

// checkLoopback cannot see other processes' sockets here; the rule that
// tool sets bind to loopback only is documented, not enforced.
func checkLoopback(pgid, port int) error { return nil }
