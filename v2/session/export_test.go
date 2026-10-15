package session

import (
	"time"

	"github.com/bwsalmon/silo/v2/gitproxy"
)

// SetMCPCallTimeout shortens the gateway's timeout for proxied MCP
// calls, for tests of blocking tools; the returned func restores it.
func SetMCPCallTimeout(d time.Duration) func() {
	old := mcpOptions
	mcpOptions.CallTimeout = d
	return func() { mcpOptions = old }
}

// SetGitCheckForwarder points silo run --dry-run's git check at a test
// upstream; the returned func restores it.
func SetGitCheckForwarder(f func(host string) (gitproxy.Forwarder, error)) func() {
	old := gitCheckForwarder
	gitCheckForwarder = f
	return func() { gitCheckForwarder = old }
}
