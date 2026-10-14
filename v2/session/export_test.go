package session

import "time"

// SetMCPCallTimeout shortens the gateway's timeout for proxied MCP
// calls, for tests of blocking tools; the returned func restores it.
func SetMCPCallTimeout(d time.Duration) func() {
	old := mcpOptions
	mcpOptions.CallTimeout = d
	return func() { mcpOptions = old }
}
