// Package ids makes silod's generated ids: six lowercase base-32
// characters, such as k7f3qa (docs/silo/README.md, "Silo", call 1).
// Silo ids use it, and so do the other records that need a short,
// stable id that is safe in a GCE resource name.
package ids

import (
	"crypto/rand"
	"strings"
)

// Len is the length of an id.
const Len = 6

// Alphabet is RFC 4648's base-32 alphabet in lower case. Every
// character is valid in a GCE resource name, and an id starts with a
// letter often enough that callers needing one prefix it ("silo-").
const Alphabet = "abcdefghijklmnopqrstuvwxyz234567"

// New returns a random id. It does not check uniqueness: the caller
// does that against its own table, in the transaction that inserts
// it, and draws again on a collision.
func New() string {
	var b [Len]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	for i := range b {
		b[i] = Alphabet[b[i]&31]
	}
	return string(b[:])
}

// Valid reports whether s has the shape of an id.
func Valid(s string) bool {
	if len(s) != Len {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(Alphabet, rune(s[i])) {
			return false
		}
	}
	return true
}
