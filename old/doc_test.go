package silo

import "testing"

func TestVersion(t *testing.T) {
	if Version != 2 {
		t.Fatalf("Version = %d, want 2", Version)
	}
}
