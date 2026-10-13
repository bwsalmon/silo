package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// stream is a hijacked call in flight: its status, and its connection.
type stream struct {
	t      *testing.T
	status int
	conn   *net.TCPConn
	r      *bufio.Reader
	body   []byte // the error's, when the call wasn't hijacked
}

// hijack makes a call that asks to upgrade to tcp, as the docker CLI
// makes attach and exec start.
func (e *env) hijack(caller, method, path string, body any, hdr map[string]string) *stream {
	e.t.Helper()
	conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close() })
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	host := "localhost"
	if h, ok := hdr["Host"]; ok {
		host = h
	}
	fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\nSilo-Caller: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n"+
		"Connection: Upgrade\r\nUpgrade: tcp\r\n\r\n%s", method, path, host, caller, len(b), b)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	s := &stream{t: e.t, status: resp.StatusCode, conn: conn.(*net.TCPConn), r: r}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		s.body, _ = io.ReadAll(resp.Body)
	} else if ct := resp.Header.Get("Content-Type"); ct != mediaMultiplexed && ct != mediaRaw {
		e.t.Errorf("%s %s: content type %q", method, path, ct)
	}
	return s
}

// send writes stdin, and with done closes the client's half.
func (s *stream) send(in string, done bool) {
	s.t.Helper()
	if _, err := s.conn.Write([]byte(in)); err != nil {
		s.t.Fatal(err)
	}
	if done {
		s.conn.CloseWrite()
	}
}

// rest is everything the server sends until it closes.
func (s *stream) rest() []byte {
	b, _ := io.ReadAll(s.r)
	return b
}

// demux splits Docker's multiplexed framing into stdout and stderr.
func demux(t *testing.T, b []byte) (stdout, stderr string) {
	t.Helper()
	var o, e strings.Builder
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatalf("a short frame header: %q", b)
		}
		n := int(binary.BigEndian.Uint32(b[4:8]))
		if len(b) < 8+n {
			t.Fatalf("a short frame: %q", b)
		}
		switch b[0] {
		case 1:
			o.Write(b[8 : 8+n])
		case 2:
			e.Write(b[8 : 8+n])
		default:
			t.Fatalf("stream %d", b[0])
		}
		b = b[8+n:]
	}
	return o.String(), e.String()
}

func TestExec(t *testing.T) {
	e := newEnv(t)
	id := e.create(e.a, "web", obj{"Env": []string{"A=2"}})
	e.fail(409, "is not running", e.a, "POST", "/v1.47/containers/web/exec", obj{"Cmd": []string{"true"}})
	e.ok(e.a, "POST", "/v1.47/containers/web/start", nil)

	var mu sync.Mutex
	var got []runtime.Process
	var sizes [][2]uint16
	e.vm.ExecFunc = func(cid string, p runtime.Process, stdio runtime.ExecIO) int {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		if p.TTY {
			for sz := range stdio.Resize {
				mu.Lock()
				sizes = append(sizes, sz)
				mu.Unlock()
				fmt.Fprintf(stdio.Stdout, "%dx%d\n", sz[0], sz[1])
				if sz[0] == 100 {
					return 0
				}
			}
			return 1
		}
		var in []byte
		if stdio.Stdin != nil {
			in, _ = io.ReadAll(stdio.Stdin)
		}
		fmt.Fprintf(stdio.Stdout, "in %s of %s", in, cid[:12])
		fmt.Fprint(stdio.Stderr, "oops")
		return 3
	}

	// Attached, with stdin: the output comes back framed, and the exit
	// code is the process's.
	x := e.ok(e.a, "POST", "/v1.47/containers/web/exec", obj{"AttachStdin": true, "AttachStdout": true, "AttachStderr": true,
		"Cmd": []string{"sh", "-c", "cat"}, "Env": []string{"B=1"}, "User": "1000", "WorkingDir": "/srv"})["Id"].(string)
	in := e.ok(e.a, "GET", "/v1.47/exec/"+x+"/json", nil)
	if in["Running"] != false || in["ExitCode"] != nil || in["ContainerID"] != id || in["OpenStdin"] != true {
		t.Errorf("inspect before start: %v", in)
	}
	s := e.hijack(e.a, "POST", "/v1.47/exec/"+x+"/start", obj{"Detach": false, "Tty": false}, nil)
	if s.status != 101 {
		t.Fatalf("start: %d %s", s.status, s.body)
	}
	s.send("hello", true)
	out, errs := demux(t, s.rest())
	if out != "in hello of "+id[:12] || errs != "oops" {
		t.Errorf("exec output: %q %q", out, errs)
	}
	in = e.ok(e.a, "GET", "/v1.47/exec/"+x+"/json", nil)
	if in["Running"] != false || in["ExitCode"] != 3.0 {
		t.Errorf("inspect after: %v", in)
	}
	p := got[0]
	if strings.Join(p.Args, " ") != "sh -c cat" || strings.Join(p.Env, ",") != "B=1" || p.User != "1000" || p.Workdir != "/srv" || !p.Stdin || p.TTY {
		t.Errorf("process: %+v", p)
	}
	e.fail(409, "has already run", e.a, "POST", "/v1.47/exec/"+x+"/start", obj{})
	// Exec ids are the silo's own.
	e.fail(404, "No such exec instance", e.b, "GET", "/v1.47/exec/"+x+"/json", nil)
	e.fail(404, "No such exec instance", e.b, "POST", "/v1.47/exec/"+x+"/start", obj{})

	// A TTY's output is raw, and its resizes reach the driver, the
	// create's ConsoleSize (height, width) first.
	x = e.ok(e.a, "POST", "/v1.47/containers/web/exec", obj{"AttachStdout": true, "Tty": true, "ConsoleSize": []int{24, 80},
		"Cmd": []string{"top"}})["Id"].(string)
	s = e.hijack(e.a, "POST", "/v1.47/exec/"+x+"/start", obj{"Tty": true}, nil)
	if s.status != 101 {
		t.Fatalf("tty start: %d %s", s.status, s.body)
	}
	line, _ := s.r.ReadString('\n')
	if line != "80x24\n" {
		t.Errorf("first size: %q", line)
	}
	e.ok(e.a, "POST", "/v1.47/exec/"+x+"/resize?h=50&w=100", nil)
	if b := s.rest(); string(b) != "100x50\n" {
		t.Errorf("tty output: %q", b)
	}
	if in := e.ok(e.a, "GET", "/v1.47/exec/"+x+"/json", nil); in["ExitCode"] != 0.0 {
		t.Errorf("tty exit: %v", in)
	}
	e.fail(400, "invalid h", e.a, "POST", "/v1.47/exec/"+x+"/resize?h=x&w=1", nil)

	// Detached, the call answers at once and the process runs on.
	x = e.ok(e.a, "POST", "/v1.47/containers/web/exec", obj{"Cmd": []string{"sleep", "1"}})["Id"].(string)
	if code, b, _ := e.raw(e.a, "POST", "/v1.47/exec/"+x+"/start", obj{"Detach": true}, nil); code != 200 {
		t.Errorf("detached start: %d %s", code, b)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.ok(e.a, "GET", "/v1.47/exec/"+x+"/json", nil)["ExitCode"] != 3.0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if in := e.ok(e.a, "GET", "/v1.47/exec/"+x+"/json", nil); in["ExitCode"] != 3.0 {
		t.Errorf("detached exit: %v", in)
	}

	e.fail(403, "a privileged exec is not allowed", e.a, "POST", "/v1.47/containers/web/exec", obj{"Cmd": []string{"sh"}, "Privileged": true})
	e.fail(400, "No exec command specified", e.a, "POST", "/v1.47/containers/web/exec", obj{})
	e.fail(400, "needs to be an absolute path", e.a, "POST", "/v1.47/containers/web/exec", obj{"Cmd": []string{"sh"}, "WorkingDir": "srv"})
	e.fail(400, "Invalid detach keys", e.a, "POST", "/v1.47/containers/web/exec", obj{"Cmd": []string{"sh"}, "DetachKeys": "ctrl-1"})
	// control=read may not exec.
	e.fail(403, "control policy is read", e.ro, "POST", "/v1.47/containers/web/exec", obj{"Cmd": []string{"sh"}})

	// A removed container's execs are forgotten.
	e.ok(e.a, "DELETE", "/v1.47/containers/web?force=1", nil)
	e.fail(404, "No such exec instance", e.a, "GET", "/v1.47/exec/"+x+"/json", nil)
}

func TestAttach(t *testing.T) {
	e := newEnv(t)
	sbx := obj{"HostConfig": obj{"Runtime": RuntimeSandbox}}
	id := e.create(e.a, "web", obj{"OpenStdin": true, "HostConfig": obj{"Runtime": RuntimeSandbox}})
	e.ok(e.a, "POST", "/v1.47/containers/web/start", nil)
	if err := e.sbx.Output(id, "hello\n", "oops\n"); err != nil {
		t.Fatal(err)
	}

	// logs=1 without stream is the output so far.
	s := e.hijack(e.a, "POST", "/v1.47/containers/web/attach?logs=1&stdout=1&stderr=1", nil, nil)
	if s.status != 101 {
		t.Fatalf("attach: %d %s", s.status, s.body)
	}
	if out, errs := demux(t, s.rest()); out != "hello\n" || errs != "oops\n" {
		t.Errorf("attach logs: %q %q", out, errs)
	}

	// stream=1 with stdin: stdin reaches the workload, and the output
	// written after the attach comes back until the workload stops.
	s = e.hijack(e.a, "POST", "/v1.47/containers/web/attach?stream=1&stdin=1&stdout=1&stderr=1", nil, nil)
	if s.status != 101 {
		t.Fatalf("attach stdin: %d %s", s.status, s.body)
	}
	s.send("input", true)
	deadline := time.Now().Add(5 * time.Second)
	for e.sbx.Stdin(id) != "input" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.sbx.Stdin(id); got != "input" {
		t.Errorf("stdin: %q", got)
	}
	e.sbx.Output(id, "later\n", "")
	time.Sleep(3 * pollOutput)
	e.sbx.Exit(id, 0)
	if out, errs := demux(t, s.rest()); out != "later\n" || errs != "" {
		t.Errorf("attach stream: %q %q", out, errs)
	}

	// docker start -a: an attach before the start sees all of the
	// workload's output.
	e.create(e.a, "job", sbx)
	s = e.hijack(e.a, "POST", "/v1.47/containers/job/attach?stream=1&stdout=1&stderr=1", nil, nil)
	e.ok(e.a, "POST", "/v1.47/containers/job/start", nil)
	job := e.ok(e.a, "GET", "/v1.47/containers/job/json", nil)["Id"].(string)
	e.sbx.Output(job, "ran\n", "")
	e.sbx.Exit(job, 0)
	if out, _ := demux(t, s.rest()); out != "ran\n" {
		t.Errorf("start -a: %q", out)
	}

	// A TTY's output is raw; its stdin ends at the detach keys, which
	// leave the workload running.
	tty := e.create(e.root, "tty", obj{"OpenStdin": true, "Tty": true, "HostConfig": obj{"Runtime": RuntimeSandbox}})
	e.ok(e.root, "POST", "/v1.47/containers/tty/start", nil)
	e.sbx.Output(tty, "$ ", "")
	s = e.hijack(e.root, "POST", "/v1.47/containers/tty/attach?logs=1&stream=1&stdin=1&stdout=1&detachKeys=ctrl-x", nil, nil)
	s.send("ls\n\x18", false)
	if b := s.rest(); string(b) != "$ " {
		t.Errorf("tty attach: %q", b)
	}
	if got := e.sbx.Stdin(tty); got != "ls\n" {
		t.Errorf("tty stdin: %q", got)
	}
	if st := e.ok(e.root, "GET", "/v1.47/containers/tty/json", nil)["State"].(obj); st["Running"] != true {
		t.Errorf("after detach: %v", st)
	}

	// Without stdin, attach is a read: control=read may, and with stdin
	// it may not.
	host := map[string]string{"Host": e.ro + ".docker.silo.internal"}
	if code, b, _ := e.raw(e.root, "POST", "/v1.47/images/create?fromImage=alpine", nil, host); code != 200 {
		t.Fatalf("pull in ro: %d %s", code, b)
	}
	code, b, _ := e.raw(e.root, "POST", "/v1.47/containers/create?name=ro", obj{"Image": "alpine", "OpenStdin": true, "HostConfig": obj{"Runtime": RuntimeSandbox}}, host)
	if code != 201 && code != 200 {
		t.Fatalf("create in ro: %d %s", code, b)
	}
	if s := e.hijack(e.ro, "POST", "/v1.47/containers/ro/attach?logs=1&stdout=1", nil, nil); s.status != 101 {
		t.Errorf("ro attach: %d %s", s.status, s.body)
	}
	if s := e.hijack(e.ro, "POST", "/v1.47/containers/ro/attach?stream=1&stdin=1&stdout=1", nil, nil); s.status != 403 ||
		!strings.Contains(string(s.body), "control policy is read") {
		t.Errorf("ro attach with stdin: %d %s", s.status, s.body)
	}
}

func TestDetachReader(t *testing.T) {
	keys, err := detachKeys("ctrl-p,ctrl-q")
	if err != nil || string(keys) != "\x10\x11" {
		t.Fatalf("keys: %q %v", keys, err)
	}
	for _, c := range []struct{ in, want string }{
		{"abc", "abc"},
		{"a\x10b", "a\x10b"},
		{"a\x10\x10\x11b", "a\x10"},
		{"\x10\x11", ""},
		{"ab\x10", "ab\x10"},
	} {
		got, err := io.ReadAll(&detachReader{r: iotest(c.in), keys: keys})
		if string(got) != c.want || (err != nil) != strings.Contains(c.in, "\x10\x11") {
			t.Errorf("%q: %q %v, want %q", c.in, got, err, c.want)
		}
	}
}

// iotest is s, a byte at a time.
func iotest(s string) io.Reader { return &oneByte{s: s} }

type oneByte struct{ s string }

func (o *oneByte) Read(p []byte) (int, error) {
	if o.s == "" {
		return 0, io.EOF
	}
	p[0] = o.s[0]
	o.s = o.s[1:]
	return 1, nil
}

func TestTop(t *testing.T) {
	e := newEnv(t)
	e.create(e.a, "web", nil)
	e.fail(409, "is not running", e.a, "GET", "/v1.47/containers/web/top", nil)
	e.ok(e.a, "POST", "/v1.47/containers/web/start", nil)
	var args []string
	e.vm.ExecFunc = func(_ string, p runtime.Process, stdio runtime.ExecIO) int {
		args = p.Args
		fmt.Fprint(stdio.Stdout, "UID PID PPID C STIME TTY TIME CMD\nroot 1 0 0 10:00 ? 00:00:00 /bin/app --flag x\nroot 7 1 0 10:01 ? 00:00:00 sh\n")
		return 0
	}
	top := e.ok(e.a, "GET", "/v1.47/containers/web/top", nil)
	ps := top["Processes"].([]any)
	if len(top["Titles"].([]any)) != 8 || len(ps) != 2 || ps[0].([]any)[7] != "/bin/app --flag x" || ps[1].([]any)[1] != "7" {
		t.Errorf("top: %v", top)
	}
	if strings.Join(args, " ") != "ps -ef" {
		t.Errorf("args: %v", args)
	}
	e.ok(e.a, "GET", "/v1.47/containers/web/top?ps_args=aux", nil)
	if strings.Join(args, " ") != "ps aux" {
		t.Errorf("args: %v", args)
	}
	// top is a read.
	if code, out := e.do(e.ro, "GET", "/v1.47/containers/x/top", nil, nil); code != 404 {
		t.Errorf("top as ro: %d %v", code, out)
	}
	e.vm.ExecFunc = func(string, runtime.Process, runtime.ExecIO) int { return 127 }
	e.fail(501, "its image has none", e.a, "GET", "/v1.47/containers/web/top", nil)
}

// tarOf is an archive of entries, in order.
func tarOf(hs ...*tar.Header) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hs {
		body := ""
		if h.Typeflag == tar.TypeReg {
			body = h.Linkname
			h.Linkname, h.Size = "", int64(len(body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		tw.WriteHeader(h)
		tw.Write([]byte(body))
	}
	tw.Close()
	return buf.Bytes()
}

// file is a regular file entry with content (carried in Linkname until
// tarOf writes it).
func file(name, content string) *tar.Header {
	return &tar.Header{Name: name, Typeflag: tar.TypeReg, Linkname: content, Uid: 1000, Gid: 1000}
}

// put is a PUT of an archive to path.
func (e *env) put(caller, ref, path string, archive []byte) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("PUT", e.srv.URL+"/v1.47/containers/"+ref+"/archive?path="+path, bytes.NewReader(archive))
	req.Header.Set("Silo-Caller", caller)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestArchive(t *testing.T) {
	e := newEnv(t)
	e.create(e.a, "web", nil)
	if code, b := e.put(e.a, "web", "/srv", tarOf(&tar.Header{Name: "d/", Typeflag: tar.TypeDir}, file("d/f", "hi"))); code != 200 {
		t.Fatalf("put: %d %s", code, b)
	}
	stat := func(h http.Header) pathStat {
		var st pathStat
		b, _ := base64.StdEncoding.DecodeString(h.Get("X-Docker-Container-Path-Stat"))
		if err := json.Unmarshal(b, &st); err != nil {
			t.Errorf("stat header %q: %v", h.Get("X-Docker-Container-Path-Stat"), err)
		}
		return st
	}
	code, _, h := e.raw(e.a, "HEAD", "/v1.47/containers/web/archive?path=/srv/d/f", nil, nil)
	if st := stat(h); code != 200 || st.Name != "f" || st.Size != 2 || st.Mode.IsDir() {
		t.Errorf("head of a file: %d %+v", code, st)
	}
	code, b, h := e.raw(e.a, "GET", "/v1.47/containers/web/archive?path=srv/../srv/d", nil, nil)
	if st := stat(h); code != 200 || st.Name != "d" || !st.Mode.IsDir() {
		t.Errorf("get of a directory: %d %+v", code, st)
	}
	var names []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hd, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		names = append(names, hd.Name+"="+string(body))
	}
	if strings.Join(names, " ") != "d/= d/f=hi" {
		t.Errorf("archive: %v", names)
	}
	if code, _, _ := e.raw(e.a, "HEAD", "/v1.47/containers/web/archive?path=/nosuch", nil, nil); code != 404 {
		t.Errorf("head of nothing: %d", code)
	}
	e.fail(404, "Could not find the file /nosuch in container web", e.a, "GET", "/v1.47/containers/web/archive?path=/nosuch", nil)
	e.fail(400, "no path given", e.a, "GET", "/v1.47/containers/web/archive", nil)
	// Another silo doesn't see it.
	e.fail(404, "No such container: web", e.b, "GET", "/v1.47/containers/web/archive?path=/srv", nil)

	// What the archive may not do.
	for _, c := range []struct {
		code    int
		want    string
		archive []byte
	}{
		{400, "leaves the destination", tarOf(file("../etc/passwd", "x"))},
		{400, "leaves the destination", tarOf(file("a/../../x", "x"))},
		{400, "leaves the destination", tarOf(&tar.Header{Name: "l", Typeflag: tar.TypeLink, Linkname: "../../etc/shadow"})},
		{400, "a symlink the archive made", tarOf(&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, file("l/passwd", "x"))},
		{403, "is a device", tarOf(&tar.Header{Name: "null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3})},
		{400, "reading the archive", []byte("this is not a tar archive, and it is long enough to have a header block's worth of nonsense in it" + strings.Repeat("!", 512))},
	} {
		if code, b := e.put(e.a, "web", "/srv", c.archive); code != c.code || !strings.Contains(b, c.want) {
			t.Errorf("put: %d %s, want %d %q", code, b, c.code, c.want)
		}
	}
	if _, ok := e.vm.Spec(e.ok(e.a, "GET", "/v1.47/containers/web/json", nil)["Id"].(string)); !ok {
		t.Fatal("no container")
	}
	code, _, _ = e.raw(e.a, "HEAD", "/v1.47/containers/web/archive?path=/srv/etc", nil, nil)
	if code != 404 {
		t.Errorf("a refused archive wrote something: %d", code)
	}

	// A read-only root takes nothing, but its writable volumes do; and
	// control=read may read and not write.
	e.create(e.a, "ro", obj{"HostConfig": obj{"ReadonlyRootfs": true}})
	if code, b := e.put(e.a, "ro", "/srv", tarOf(file("f", "x"))); code != 403 || !strings.Contains(b, "read-only filesystem") {
		t.Errorf("put to a read-only root: %d %s", code, b)
	}
	if code, b := e.put(e.a, "ro", "/data/sub", tarOf(file("f", "x"))); code != 200 {
		t.Errorf("put to a volume of a read-only root: %d %s", code, b)
	}
}

func TestCopyArchive(t *testing.T) {
	var out bytes.Buffer
	in := tarOf(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}, file("/abs/f", "hello"),
		&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, &tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "abs/f"})
	if err := copyArchive(&out, bytes.NewReader(in), 1<<20, false); err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got = append(got, fmt.Sprintf("%s %c %s %d:%d", h.Name, h.Typeflag, h.Linkname, h.Uid, h.Gid))
	}
	if want := "abs/f 0  0:0,l 2 /etc/passwd 0:0,h 1 abs/f 0:0"; strings.Join(got, ",") != want {
		t.Errorf("rewritten: %q, want %q", strings.Join(got, ","), want)
	}
	out.Reset()
	if err := copyArchive(&out, bytes.NewReader(in), 1<<20, true); err != nil {
		t.Fatal(err)
	}
	if h, err := tar.NewReader(&out).Next(); err != nil || h.Uid != 1000 || h.Gid != 1000 {
		t.Errorf("keeping owners: %+v %v", h, err)
	}
	if err := copyArchive(io.Discard, bytes.NewReader(tarOf(file("a", "12"), file("b", "34"))), 3, false); err != errTooBig {
		t.Errorf("over the cap: %v", err)
	}
}
