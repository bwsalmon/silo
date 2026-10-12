package ids

import "testing"

func TestNew(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		id := New()
		if !Valid(id) {
			t.Fatalf("New() = %q, not a valid id", id)
		}
		seen[id] = true
	}
	// 30 bits of randomness: 2000 draws collide with odds of about 0.2%.
	if len(seen) < 1990 {
		t.Errorf("%d distinct ids in 2000 draws", len(seen))
	}
}

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"k7f3qa":  true,
		"aaaaaa":  true,
		"222777":  true,
		"K7F3QA":  false,
		"k7f3q":   false,
		"k7f3qaa": false,
		"k1f3qa":  false, // 1 is not in the alphabet
		"k8f3qa":  false,
		"k-f3qa":  false,
		"":        false,
	} {
		if got := Valid(s); got != want {
			t.Errorf("Valid(%q) = %v, want %v", s, got, want)
		}
	}
}
