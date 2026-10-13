package nsd

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAdopt restarts the holder the way silo-nsd.service does (call
// L33): the holder dies while a member (standing in for containerd)
// is still in its namespaces, and a new holder adopts them, keeping
// the container pins, silo-net and gw0.
func TestAdopt(t *testing.T) {
	c, cfg, pid, m := startHolder(t)
	if _, err := c.Container("c1", 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Pid != pid || st.NS.User == "" || st.NS.Net == "" {
		t.Fatalf("nsd.state = %s, want pid %d and its namespaces", b, pid)
	}
	ours, _ := ReadNSIDs(os.Getpid())
	if st.NS.User == ours.User || st.NS.Mnt == ours.Mnt || st.NS.Net == ours.Net {
		t.Fatalf("the holder's namespaces %+v include the test's %+v", st.NS, ours)
	}

	t.Run("refused while the holder answers", func(t *testing.T) {
		if _, _, err := Adopt(os.Args[0], nil, nil, st, cfg.Socket, m, os.Stderr, os.Stderr); !errors.Is(err, ErrHolderRunning) {
			t.Fatalf("Adopt = %v, want ErrHolderRunning", err)
		}
	})

	member := JoinCommand(pid, os.Args[0], "-test.run=^$")
	member.Env = append(member.Env, "_SILO_NSD_TEST_EXEC=sleep 600")
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { member.Process.Kill(); member.Wait() })
	for i := 0; ; i++ {
		if ids, err := ReadNSIDs(member.Process.Pid); err == nil && ids == st.NS {
			break
		}
		if i > 100 {
			t.Fatal("the member never joined the holder's namespaces")
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.Close()
	p, _ := os.FindProcess(pid)
	p.Kill()
	for i := 0; holderAnswers(cfg.Socket) || processExists(pid); i++ {
		if i > 100 {
			t.Fatal("the holder did not die")
		}
		time.Sleep(50 * time.Millisecond)
	}

	got, err := Survivor(st)
	if err != nil || got != member.Process.Pid {
		t.Fatalf("Survivor = %d, %v; want the member %d", got, err, member.Process.Pid)
	}

	t.Run("refused under another mapping", func(t *testing.T) {
		other := m
		other.UID = append([]Range{}, m.UID...)
		other.UID[len(other.UID)-1].Size--
		if _, _, err := Adopt(os.Args[0], nil, nil, st, cfg.Socket, other, os.Stderr, os.Stderr); err == nil || !strings.Contains(err.Error(), "map") {
			t.Fatalf("Adopt under another mapping = %v, want a refusal naming the mapping", err)
		}
	})

	acfg := cfg
	acfg.Adopt = true
	ab, _ := json.Marshal(acfg)
	cmd, via, err := Adopt(os.Args[0], []string{"-test.run=^$"}, append(os.Environ(), "_SILO_NSD_TEST_HOLDER="+string(ab)), st, cfg.Socket, m, os.Stderr, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if via != member.Process.Pid {
		t.Errorf("adopted through %d, want the member %d", via, member.Process.Pid)
	}
	var c2 *Client
	for i := 0; ; i++ {
		if c2, err = Dial(cfg.Socket); err == nil {
			break
		}
		if i > 100 {
			t.Fatalf("the adopting holder never listened: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer c2.Close()
	info, err := c2.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Pid != cmd.Process.Pid {
		t.Errorf("Info().Pid = %d, want the new holder %d", info.Pid, cmd.Process.Pid)
	}
	ids, err := ReadNSIDs(cmd.Process.Pid)
	if err != nil || ids != st.NS {
		t.Fatalf("the new holder is in %+v (%v), want the old namespaces %+v", ids, err, st.NS)
	}
	if b, err = os.ReadFile(cfg.State); err != nil || !strings.Contains(string(b), `"pid":`+strconv.Itoa(cmd.Process.Pid)) {
		t.Errorf("nsd.state after adoption = %s, %v", b, err)
	}
	// The pin survived, on the tmpfs the first holder mounted, and gw0
	// kept its address.
	out := inHolder(t, cmd.Process.Pid, t.TempDir(), "test -e "+cfg.PinDir+"/c1.net && echo pinned\nip -br addr show gw0\n")
	if !strings.Contains(out, "pinned") || !strings.Contains(out, cfg.Addrs[0].String()) {
		t.Errorf("after adoption: %s", out)
	}
	// And it serves: a second container is pinned beside the first.
	if _, err := c2.Container("c2", 0); err != nil {
		t.Fatal(err)
	}
}

func processExists(pid int) bool {
	err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run()
	return err == nil && !zombie(pid)
}

func zombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || i+2 >= len(s) || s[i+2] == 'Z'
}
