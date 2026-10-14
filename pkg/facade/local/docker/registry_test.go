package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/store"
)

// The facade's half of the local registry (local-registry.md §7, call
// L68): the whiteout check its commit makes. The facade cases of §12
// are the conformance suite's, in pkg/conformance/docker, which runs
// them against this facade over the fake drivers (local/fake) and
// against the installed local targets.

// login is the test's upstream: good/creds is the one credential that
// works.
func login(_ context.Context, _, server, user, pass string) error {
	if user == "good" && pass == "creds" {
		return nil
	}
	return errors.New("unauthorized")
}

// auditOf is every audit record of kind, with its silo as _silo.
func (e *env) auditOf(kind string) []obj {
	e.t.Helper()
	var out []obj
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT silo, fields FROM audit WHERE kind = ? ORDER BY n`, kind)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var silo, fields string
			if err := rows.Scan(&silo, &fields); err != nil {
				return err
			}
			f := obj{}
			json.Unmarshal([]byte(fields), &f)
			f["_silo"] = silo
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// The whiteout check: overlayfs's whiteouts and opaque directories
// become OCI's, and a device or a path outside the root is refused.
func TestWhiteoutLayer(t *testing.T) {
	mk := func(hs ...*tar.Header) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range hs {
			tw.WriteHeader(h)
		}
		tw.Close()
		return buf.Bytes()
	}
	var out bytes.Buffer
	err := WhiteoutLayer(&out, bytes.NewReader(mk(
		&tar.Header{Name: "./a/gone", Typeflag: tar.TypeChar},
		&tar.Header{Name: "./opq", Typeflag: tar.TypeDir, Mode: 0o755, PAXRecords: map[string]string{"SCHILY.xattr.trusted.overlay.opaque": "y"}},
	)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if len(h.PAXRecords) > 0 {
			t.Errorf("%s keeps %v", h.Name, h.PAXRecords)
		}
	}
	if want := []string{"a/.wh.gone", "opq/", "opq/.wh..wh..opq"}; !slices.Equal(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for _, h := range []*tar.Header{
		{Name: "dev/sda", Typeflag: tar.TypeBlock, Devmajor: 8},
		{Name: "dev/null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "x", Typeflag: tar.TypeLink, Linkname: "../../etc/shadow"},
	} {
		if err := WhiteoutLayer(io.Discard, bytes.NewReader(mk(h))); err == nil {
			t.Errorf("%s was not refused", h.Name)
		}
	}
}

// What runsc release-20261005 wrote for a container that removed
// /etc/motd and /etc/os-release, replaced /etc/apk/keys, and made /new
// owned by 1234 (testdata/rootfs-upper.tar, from `runsc tar rootfs-upper`;
// local-registry.md §13, step 35): the second whiteout is a hard link to
// the first, and comes out as a whiteout of its own.
func TestWhiteoutLayerRunsc(t *testing.T) {
	in, err := os.ReadFile("testdata/rootfs-upper.tar")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WhiteoutLayer(&out, bytes.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got = append(got, fmt.Sprintf("%c %d:%d %s", h.Typeflag, h.Uid, h.Gid, h.Name))
	}
	want := []string{
		"5 0:0 etc/",
		"0 0:0 etc/.wh.motd",
		"5 0:0 etc/apk/",
		"5 0:0 etc/apk/keys/",
		"0 0:0 etc/apk/keys/.wh..wh..opq",
		"0 0:0 etc/apk/keys/mine",
		"0 0:0 etc/.wh.os-release",
		"0 1234:1234 new",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
