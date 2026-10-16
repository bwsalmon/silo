package testlog

import "testing"

func TestLogfAfterCleanup(t *testing.T) {
	var logf func(string, ...any)
	t.Run("inner", func(t *testing.T) {
		logf = Logf(t)
		logf("during the test")
	})
	// The inner test has finished; t.Logf would panic now.
	logf("after the test")
}
