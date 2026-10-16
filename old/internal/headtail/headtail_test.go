// Adapted from github.com/bwsalmon/grain pkg/mcp/exec_output_test.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package headtail

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestKeepsEverythingThatFits(t *testing.T) {
	b := New(100)
	var want strings.Builder
	for i := 0; ; i++ {
		s := fmt.Sprintf("%d,", i)
		if want.Len()+len(s) > 100 {
			break
		}
		want.WriteString(s)
		b.Write([]byte(s))
	}
	if got := string(b.Bytes()); got != want.String() || b.Dropped() != 0 {
		t.Fatalf("Bytes() = %q (dropped %d), want %q", got, b.Dropped(), want.String())
	}
}

func TestKeepsHeadAndTailOfWhatDoesNot(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, limit := range []int{1, 2, 7, 64, 1000} {
		for trial := 0; trial < 50; trial++ {
			b := New(limit)
			var all []byte
			for len(all) < 5*limit+rng.Intn(3*limit+1) {
				chunk := make([]byte, 1+rng.Intn(2*limit))
				rng.Read(chunk)
				all = append(all, chunk...)
				b.Write(chunk)
			}
			head, tail := all[:limit/2], all[len(all)-(limit-limit/2):]
			got := b.Bytes()
			if !bytes.HasPrefix(got, head) || !bytes.HasSuffix(got, tail) {
				t.Fatalf("limit %d: head or tail lost", limit)
			}
			if want := int64(len(all) - limit); b.Dropped() != want {
				t.Fatalf("limit %d: Dropped() = %d, want %d", limit, b.Dropped(), want)
			}
		}
	}
}
