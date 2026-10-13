package targets

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/fake"
	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/facade/local/docker"
	"github.com/bwsalmon/silo/pkg/store"
)

// shellDriver is a fake driver whose containers run a scripted shell,
// for the compose and testcontainers cases (local-backend.md §5): a
// workload's `sh -c` script runs when it starts, writing its output
// and exiting with its status unless it ends in `sleep infinity`, and
// an exec runs the same way. Nothing else of the image runs, which
// Simulated says.
type shellDriver struct {
	*fake.Driver
	f *fakeRegistry
}

func newShellDriver(f *fakeRegistry, class runtime.Class, volumes string) shellDriver {
	d := shellDriver{Driver: fake.New(class), f: f}
	d.Volumes = volumes
	d.ExecFunc = func(id string, p runtime.Process, stdio runtime.ExecIO) int {
		out, errOut, code, _ := d.run(id, p.Args)
		if stdio.Stdout != nil {
			io.WriteString(stdio.Stdout, out)
		}
		if stdio.Stderr != nil {
			io.WriteString(stdio.Stderr, errOut)
		}
		return code
	}
	return d
}

// Start implements runtime.Driver: the workload's script runs once the
// facade's call has returned, as a real process would carry on after
// it.
func (d shellDriver) Start(ctx context.Context, id string) error {
	if err := d.Driver.Start(ctx, id); err != nil {
		return err
	}
	spec, ok := d.Spec(id)
	if !ok {
		return nil
	}
	go func() {
		out, errOut, code, forever := d.run(id, spec.Process.Args)
		if d.Output(id, out, errOut) == nil && !forever {
			d.Exit(id, code)
		}
	}()
	return nil
}

// run runs args as the fake shell: `sh -c <script>`, or one command.
// forever is a script that ended in `sleep infinity`.
func (d shellDriver) run(id string, args []string) (stdout, stderr string, code int, forever bool) {
	script := strings.Join(args, " ")
	if len(args) == 3 && (args[0] == "sh" || args[0] == "/bin/sh") && args[1] == "-c" {
		script = args[2]
	}
	var out, errOut strings.Builder
	// A script is commands joined by ; and &&, each of which runs only
	// if the one before it succeeded.
	for _, part := range strings.Split(script, ";") {
		for i, cmd := range strings.Split(part, "&&") {
			if i > 0 && code != 0 {
				break
			}
			words := splitWords(cmd)
			if len(words) > 0 && words[0] == "exec" {
				words = words[1:]
			}
			if len(words) == 0 {
				continue
			}
			if words[0] == "sleep" && len(words) == 2 && words[1] == "infinity" {
				return out.String(), errOut.String(), 0, true
			}
			var exit bool
			code, exit = d.command(id, words, &out, &errOut)
			if exit {
				return out.String(), errOut.String(), code, false
			}
		}
	}
	return out.String(), errOut.String(), code, false
}

// command runs one of the fake shell's commands.
func (d shellDriver) command(id string, w []string, out, errOut io.Writer) (code int, exit bool) {
	switch w[0] {
	case "echo":
		fmt.Fprintln(out, strings.Join(w[1:], " "))
	case "true", "sleep", "sh", "/bin/sh":
		// A shell with no script reads its stdin, which a workload
		// started without -i doesn't have, and exits 0.
	case "false":
		return 1, false
	case "exit":
		if len(w) > 1 {
			code, _ = strconv.Atoi(w[1])
		}
		return code, true
	case "cat", "test":
		p := w[len(w)-1]
		b, ok := d.file(id, p)
		if !ok {
			if w[0] == "cat" {
				fmt.Fprintf(errOut, "cat: can't open '%s': No such file or directory\n", p)
			}
			return 1, false
		}
		if w[0] == "cat" {
			out.Write(b)
		}
	case "ps":
		// The workload is the only process, as ps -ef shows it.
		args := []string{"sh"}
		for _, dd := range d.f.drivers {
			if spec, ok := dd.(shellDriver).Spec(id); ok {
				args = spec.Process.Args
			}
		}
		fmt.Fprintf(out, "UID PID PPID C STIME TTY TIME CMD\nroot 1 0 0 00:00 ? 00:00:00 %s\n", strings.Join(args, " "))
	case "nc":
		// nc -z [-w N] host port: whether a TCP connection to host's
		// port would be made, as the fence decides it.
		if len(w) < 4 || w[1] != "-z" {
			fmt.Fprintln(errOut, "nc: the fake shell runs only nc -z [-w N] host port")
			return 2, false
		}
		port, _ := strconv.Atoi(w[len(w)-1])
		if why := d.f.connect(id, w[len(w)-2], port); why != "" {
			fmt.Fprintf(errOut, "nc: %s:%d: %s\n", w[len(w)-2], port, why)
			return 1, false
		}
	default:
		fmt.Fprintf(errOut, "sh: %s: not found\n", w[0])
		return 127, false
	}
	return 0, false
}

// file is path's content in the container, as CopyIn put it there.
func (d shellDriver) file(id, path string) ([]byte, bool) {
	rc, err := d.CopyOut(context.Background(), id, path)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, false
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			return b, true
		}
	}
}

// splitWords splits a command into words, with ” and "" quoting.
func splitWords(s string) []string {
	var words []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words
}

// connect is why a TCP connection from container id to host's port
// would fail, or "": host resolved by the gateway's DNS as the
// container asks it, and the connection judged by the fence, as a
// probe's check is. The container's own ports all listen, as a probe's
// do.
func (f *fakeRegistry) connect(id, host string, port int) string {
	if host == "localhost" || host == "127.0.0.1" {
		return ""
	}
	silo, addrs, err := f.addrsOf(id)
	if err != nil {
		return err.Error()
	}
	if len(addrs) == 0 {
		return "the container has no address"
	}
	dst, err := netip.ParseAddr(host)
	if err != nil {
		ctx := context.Background()
		names := docker.Names{API: gatewayAPI, Docker: gatewayDocker}
		for _, from := range addrs {
			var ans docker.Answer
			if err := f.db.Tx(ctx, func(tx store.Tx) error {
				var err error
				ans, err = f.facade.Lookup(ctx, tx, names, silo, from, host)
				return err
			}); err != nil {
				return err.Error()
			}
			if len(ans.Addrs) > 0 {
				dst = ans.Addrs[0]
				break
			}
		}
		if !dst.IsValid() {
			return "bad address: NXDOMAIN"
		}
	}
	res := f.probeSend(&fakeProbe{f: f, silo: silo, addr: addrs[0]}, "", addrs[0],
		[]conformance.Check{{ID: "c", Kind: "tcp", Host: dst.String(), Port: port}})["c"]
	if !res.OK {
		return res.Detail
	}
	return ""
}

// addrsOf is container id's silo and its addresses, from the facade as
// the root's members see it.
func (f *fakeRegistry) addrsOf(id string) (string, []netip.Addr, error) {
	var silo string
	for _, d := range []runtime.Driver{f.drivers[0], f.drivers[1]} {
		if spec, ok := d.(shellDriver).Spec(id); ok {
			silo = spec.Silo
		}
	}
	if silo == "" {
		return "", nil, fmt.Errorf("no container %s", id)
	}
	req, _ := http.NewRequest("GET", f.dockSrv.URL+"/v1.47/containers/"+id+"/json", nil)
	req.Host = silo + "." + docker.Domain
	req.Header.Set("Silo-Caller", f.root)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	var in struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err := json.NewDecoder(res.Body).Decode(&in); err != nil {
		return "", nil, err
	}
	var out []netip.Addr
	for _, n := range in.NetworkSettings.Networks {
		if a, err := netip.ParseAddr(n.IPAddress); err == nil {
			out = append(out, a)
		}
	}
	return silo, out, nil
}

// Simulated implements conformance.Simulated.
func (f *fakeRegistry) Simulated() string {
	return "local/fake's containers run a scripted shell (echo, cat, test, nc -z, ps, sleep), not their images"
}
