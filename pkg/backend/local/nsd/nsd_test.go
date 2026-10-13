package nsd

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// The uid check runs without namespaces: a server that allows another
// uid refuses this process, and one that allows it serves.
func TestServerChecksPeerUID(t *testing.T) {
	for _, tc := range []struct {
		allow uint32
		ok    bool
	}{{uint32(os.Getuid()) + 1, false}, {uint32(os.Getuid()), true}} {
		sock := filepath.Join(t.TempDir(), "nsd.sock")
		l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: sock, Net: "unixpacket"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		s := &Server{AllowUID: tc.allow, Mapping: Mapping{UID: []Range{{0, 1000, 1}}}}
		go s.Serve(ctx, l)
		c, err := Dial(sock)
		if err != nil {
			t.Fatal(err)
		}
		info, err := c.Info()
		if tc.ok && (err != nil || info.Pid != os.Getpid()) {
			t.Errorf("allowed uid: info = %+v, %v", info, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "is not silod's")) {
			t.Errorf("other uid: info = %+v, %v; want a refusal", info, err)
		}
		if tc.ok {
			if _, _, err := c.call(Request{Op: "exec"}); err == nil {
				t.Error("an unknown op was served")
			}
			if _, err := c.Netlink("xfrm"); err == nil {
				t.Error("an unlisted netlink protocol was served")
			}
			if _, err := c.DialSockets("tcp4", maxDialBatch+1); err == nil {
				t.Error("an oversized dial batch was served")
			}
		}
		c.Close()
		cancel()
	}
}

func TestCheckMapping(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx, st.Latest()); err != nil {
		t.Fatal(err)
	}
	m := HolderMapping(1000, 1000, SubID{100000, 65536}, SubID{100000, 65536})
	if m.String() != "uid=0:1000:1,1:100000:65536 gid=0:1000:1,1:100000:65536" {
		t.Fatalf("String = %q", m)
	}
	if err := CheckMapping(ctx, st, m); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := CheckMapping(ctx, st, m); err != nil {
		t.Fatalf("same mapping: %v", err)
	}
	moved := HolderMapping(1000, 1000, SubID{200000, 65536}, SubID{100000, 65536})
	err = CheckMapping(ctx, st, moved)
	if !errors.Is(err, ErrMappingChanged) || !strings.Contains(err.Error(), "1:200000:65536") {
		t.Fatalf("changed mapping: %v", err)
	}
}

func TestParseAndRanges(t *testing.T) {
	sub, ok, err := ParseSubID(strings.NewReader("# c\nalice:100000:65536\nbob:165536:131072\n"), "bob", 1001)
	if err != nil || !ok || sub != (SubID{165536, 131072}) {
		t.Fatalf("ParseSubID = %+v, %v, %v", sub, ok, err)
	}
	if _, ok, _ := ParseSubID(strings.NewReader("1002:1:65536\n"), "carol", 1002); !ok {
		t.Fatal("a line naming the uid was not found")
	}
	rs, err := ParseIDMap(strings.NewReader("         0       1000          1\n         1     100000      65536\n"))
	if err != nil || len(rs) != 2 || rs[1] != (Range{1, 100000, 65536}) {
		t.Fatalf("ParseIDMap = %+v, %v", rs, err)
	}
	m := HolderMapping(1000, 1000, sub, sub)
	if r, err := ContainerRange(m, 1); err != nil || r != (Range{0, 65537, 65536}) {
		t.Fatalf("range 1 = %+v, %v", r, err)
	}
	if _, err := ContainerRange(m, 2); err == nil {
		t.Fatal("range 2 of 2 was accepted")
	}
}

type fakeInfo struct {
	fs.FileInfo
	mode fs.FileMode
}

func (f fakeInfo) Mode() fs.FileMode { return f.mode }

func TestProbeHost(t *testing.T) {
	files := map[string]string{
		"/proc/sys/kernel/unprivileged_userns_clone":             "1\n",
		"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "1\n",
		"/etc/subuid": "silo:100000:65536\n",
		"/etc/subgid": "silo:1000:65536\n",
	}
	h := Host{
		ReadFile: func(n string) ([]byte, error) {
			if v, ok := files[n]; ok {
				return []byte(v), nil
			}
			return nil, fs.ErrNotExist
		},
		LookPath: func(f string) (string, error) {
			if f == "newuidmap" {
				return "/usr/bin/newuidmap", nil
			}
			return "", errors.New("not found")
		},
		Stat:      func(string) (fs.FileInfo, error) { return fakeInfo{mode: fs.ModeSetuid | 0o755}, nil },
		User:      "silo",
		UID:       1000,
		GID:       1000,
		TryUserNS: func() error { return errors.New("operation not permitted") },
	}
	got := map[string]Check{}
	for _, c := range ProbeHost(h) {
		got[c.Name] = c
	}
	for name, ok := range map[string]bool{
		"unprivileged user namespaces":        false,
		"AppArmor user namespace restriction": false,
		"newuidmap":                           true,
		"newgidmap":                           false,
		"range in /etc/subuid":                true,
		"range in /etc/subgid":                false, // contains the user's own gid
	} {
		c, found := got[name]
		if !found || c.OK != ok || (!ok && c.Fix == "" && name != "range in /etc/subgid") {
			t.Errorf("%s = %+v, want ok %v with a fix", name, c, ok)
		}
	}
	h.TryUserNS = func() error { return nil }
	for _, c := range ProbeHost(h) {
		if c.Name == "AppArmor user namespace restriction" && !c.OK {
			t.Errorf("a binary its profile allows: %+v", c)
		}
	}
}
