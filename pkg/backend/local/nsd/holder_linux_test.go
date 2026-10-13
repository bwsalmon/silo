package nsd

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	_ "github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
)

// TestMain doubles as the holder (Start re-runs this binary with
// _SILO_NSD_TEST_HOLDER) and as a command run inside it
// (_SILO_NSD_TEST_EXEC, after nsexec's join stage).
func TestMain(m *testing.M) {
	if cfg := os.Getenv("_SILO_NSD_TEST_HOLDER"); cfg != "" {
		var c Config
		if err := json.Unmarshal([]byte(cfg), &c); err != nil {
			panic(err)
		}
		if err := Run(context.Background(), c); err != nil {
			os.Stderr.WriteString("holder: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	if args := os.Getenv("_SILO_NSD_TEST_EXEC"); args != "" {
		argv := strings.Fields(args)
		p, err := exec.LookPath(argv[0])
		if err == nil {
			err = syscall.Exec(p, argv, os.Environ())
		}
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(127)
	}
	os.Exit(m.Run())
}

// holderMapping is the mapping this host gives the current user, or a
// skip (a failure under SILO_REQUIRE_NSD) when it can't hold one.
func holderMapping(t *testing.T) Mapping {
	t.Helper()
	var missing []string
	for _, c := range Failed(Probe()) {
		missing = append(missing, c.Name+": "+c.Detail)
	}
	if len(missing) > 0 {
		if os.Getenv("SILO_REQUIRE_NSD") != "" {
			t.Fatalf("SILO_REQUIRE_NSD is set, but this host can't run the holder: %s", strings.Join(missing, "; "))
		}
		t.Skipf("this host can't run the holder: %s", strings.Join(missing, "; "))
	}
	sub := func(file string, id int) SubID {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		s, _, err := ParseSubID(f, currentUser(), id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	return HolderMapping(os.Getuid(), os.Getgid(), sub("/etc/subuid", os.Getuid()), sub("/etc/subgid", os.Getgid()))
}

func startHolder(t *testing.T) (*Client, Config, int, Mapping) {
	m := holderMapping(t)
	dir := t.TempDir()
	cfg := Config{
		Socket: filepath.Join(dir, "nsd.sock"), State: filepath.Join(dir, "nsd.state"),
		PinDir: filepath.Join(dir, "netns"), Silos: filepath.Join(dir, "silos"),
		Bridge: "gw0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.99.0.1/16")},
	}
	b, _ := json.Marshal(cfg)
	cmd, err := Start(os.Args[0], []string{"-test.run=^$"}, append(os.Environ(), "_SILO_NSD_TEST_HOLDER="+string(b)), m, os.Stderr, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var c *Client
	for i := 0; ; i++ {
		if c, err = Dial(cfg.Socket); err == nil {
			break
		}
		if i > 100 {
			t.Fatalf("holder never listened: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { c.Close() })
	return c, cfg, cmd.Process.Pid, m
}

// nftNewTable sends an nf_tables batch creating table inet <name> on a
// NETLINK_NETFILTER socket and returns the kernel's verdict.
func nftNewTable(fd int, name string) error {
	msg := func(typ uint16, flags uint16, seq uint32, body []byte) []byte {
		b := make([]byte, 16, 16+len(body))
		binary.NativeEndian.PutUint32(b, uint32(16+len(body)))
		binary.NativeEndian.PutUint16(b[4:], typ)
		binary.NativeEndian.PutUint16(b[6:], flags)
		binary.NativeEndian.PutUint32(b[8:], seq)
		return append(b, body...)
	}
	gen := func(fam byte, res uint16) []byte { return []byte{fam, 0, byte(res >> 8), byte(res)} }
	const nft = unix.NFNL_SUBSYS_NFTABLES
	var batch []byte
	batch = append(batch, msg(unix.NFNL_MSG_BATCH_BEGIN, unix.NLM_F_REQUEST, 1, gen(0, nft))...)
	batch = append(batch, msg(nft<<8|unix.NFT_MSG_NEWTABLE, unix.NLM_F_REQUEST|unix.NLM_F_ACK|unix.NLM_F_CREATE, 2,
		append(gen(unix.NFPROTO_INET, 0), attrs(nil).str(unix.NFTA_TABLE_NAME, name)...))...)
	batch = append(batch, msg(unix.NFNL_MSG_BATCH_END, unix.NLM_F_REQUEST, 3, gen(0, nft))...)
	if err := unix.Sendto(fd, batch, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 1<<16)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Header.Type == unix.NLMSG_ERROR {
			if code := int32(binary.NativeEndian.Uint32(m.Data)); code != 0 {
				return unix.Errno(-code)
			}
			return nil
		}
	}
	return errors.New("no acknowledgement")
}

func TestHolder(t *testing.T) {
	c, cfg, pid, m := startHolder(t)

	info, err := c.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Pid == 0 || info.Mapping.String() != m.String() || info.Driver != storage.DirName {
		t.Fatalf("info = %+v, want mapping %s and the dir driver", info, m)
	}
	if st, err := ReadState(cfg.State); err != nil || st.Pid != pid {
		t.Fatalf("ReadState = %+v, %v; want pid %d", st, err, pid)
	}

	t.Run("netlink from silod through a passed socket", func(t *testing.T) {
		f, err := c.Netlink("route")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r := &rtnl{fd: int(f.Fd())}
		if err := r.bridge("br-silod", false); err != nil {
			t.Fatalf("silod creating a bridge in silo-net: %v", err)
		}
		if _, err := r.index("br-silod"); err != nil {
			t.Fatal(err)
		}
		if _, err := net.InterfaceByName("br-silod"); err == nil {
			t.Fatal("br-silod is in the host's netns, not silo-net")
		}
	})

	t.Run("nftables from silod through a passed socket", func(t *testing.T) {
		f, err := c.Netlink("netfilter")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := nftNewTable(int(f.Fd()), "silo"); err != nil {
			t.Fatalf("silod creating table inet silo in silo-net: %v", err)
		}
	})

	t.Run("listening socket and dial pool", func(t *testing.T) {
		// Port 80: the holder may bind under 1024 in silo-net.
		l, err := c.Listen("tcp4", "10.99.0.1:80")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		go func() {
			if a, err := l.Accept(); err == nil {
				io.Copy(a, a)
				a.Close()
			}
		}()
		pool := NewPool(c, "tcp4", 4)
		defer pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := pool.Dial(ctx, netip.MustParseAddrPort("10.99.0.1:0"), netip.MustParseAddrPort("10.99.0.1:80"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write([]byte("ping"))
		buf := make([]byte, 4)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("echo = %q, %v", buf, err)
		}
	})

	t.Run("container namespaces", func(t *testing.T) {
		ns, err := c.Container("c1", 0)
		if err != nil {
			t.Fatal(err)
		}
		if ns.Range != (Range{0, 1, 65536}) {
			t.Fatalf("range = %+v", ns.Range)
		}
		if _, err := c.Container("c1", 0); err != nil {
			t.Fatalf("an existing container's namespaces: %v", err)
		}
		if _, err := c.Container("c2", 1<<20); err == nil {
			t.Fatal("a range outside the holder's mapping was accepted")
		}
		if err := c.Veth(Veth{Container: "c1", Name: "vc1", Bridge: "gw0", MAC: "02:00:0a:63:00:02",
			Addrs: []netip.Prefix{netip.MustParsePrefix("10.99.0.2/16")}, Gateway: netip.MustParseAddr("10.99.0.1")}); err != nil {
			t.Fatal(err)
		}
		// Nothing listens in the container: a refusal, not a timeout,
		// shows the veth carries traffic.
		pool := NewPool(c, "tcp4", 1)
		defer pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Dial(ctx, netip.AddrPort{}, netip.MustParseAddrPort("10.99.0.2:9")); !errors.Is(err, unix.ECONNREFUSED) {
			t.Fatalf("dialling the container = %v, want connection refused", err)
		}
		if err := c.DeleteContainer("c1", "vc1"); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteContainer("c1", "vc1"); err != nil {
			t.Fatalf("deleting again: %v", err)
		}
	})

	t.Run("storage behind the RPC", func(t *testing.T) {
		d, err := c.Storage()
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if _, err := d.Create(ctx, storage.Ref{Silo: "s1"}, 0); err != nil {
			t.Fatal(err)
		}
		ref := storage.Ref{Silo: "s1", Volume: "v1"}
		p, err := d.Create(ctx, ref, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "f"), bytes.Repeat([]byte("x"), 8192), 0o644); err != nil {
			t.Fatal(err)
		}
		if u, err := d.Usage(ctx, ref); err != nil || u.Bytes < 8192 || u.Inodes != 2 {
			t.Fatalf("usage = %+v, %v", u, err)
		}
		if _, err := d.Create(ctx, storage.Ref{Silo: "../x"}, 0); err == nil {
			t.Fatal("an invalid ref was accepted")
		}
		if err := d.Delete(ctx, ref); err != nil {
			t.Fatal(err)
		}
	})
}
