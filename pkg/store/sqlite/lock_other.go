//go:build !unix

package sqlite

// lockFile is a no-op where flock is unavailable: the single-process
// rule is then the operator's to keep.
func lockFile(string) (func() error, error) {
	return func() error { return nil }, nil
}
