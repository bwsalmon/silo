// Adapted from github.com/bwsalmon/grain pkg/gitproxy/local.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// A git upstream on local disk: git http-backend serving a directory of
// bare repositories, behind the Forwarder interface. grain serves its own
// state repository this way; here it is the upstream the package's tests
// (and silo's) push to and clone from with real git clients, in place of
// GitHub. Every push is checked by receive-pack under receiveHardening
// rather than under git's own defaults, which validate nothing a client
// sends.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// receiveHardening is the receive-pack configuration every request to a
// local repository runs under, passed through GIT_CONFIG_* so it holds no
// matter what the repository's own config says and is not something a
// change to that config can turn off.
//
// git's defaults accept any object a pack carries without parsing it
// (receive.fsckObjects is off), so a sandbox could push trees git itself
// would refuse to write -- entries named .git or .., duplicate or
// unsorted names, malformed modes -- and commits whose headers do not
// parse, all of which later readers of the checkout trust. fsckObjects
// makes index-pack check every object it receives and refuse the push
// on the first bad one, before any ref moves. Strict, as receive-pack
// runs it, refuses everything git ranks a warning or worse -- but git
// ranks a file mode it does not know (badFilemode: 100666, 123456) as
// only "info", which strict mode still lets through, so that one is
// raised to an error by name. receive.fsck.<id> is how; an id git does
// not know is fatal, and this one has existed since the setting did.
//
// The other two keep a push off the branch a non-bare repository has
// checked out whatever its config happens to say: no update to it, no
// deletion of it, and nothing about either left to a default.
var receiveHardening = [][2]string{
	{"receive.fsckObjects", "true"},
	{"transfer.fsckObjects", "true"},
	{"receive.fsck.badFilemode", "error"},
	{"receive.denyCurrentBranch", "refuse"},
	{"receive.denyDeleteCurrent", "refuse"},
}

func receiveHardeningEnv() []string {
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(receiveHardening))}
	for i, kv := range receiveHardening {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// LocalForwarder answers smart-HTTP requests from the repositories under
// Root, Root/<owner>/<name>.git, by running git http-backend under
// receiveHardening. It is a Forwarder and a StreamForwarder, and records
// every call's credential so a test can see what would have gone
// upstream.
type LocalForwarder struct {
	Root string

	mu    sync.Mutex
	calls []ForwardCall
}

// Calls is every request forwarded so far, without bodies.
func (f *LocalForwarder) Calls() []ForwardCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ForwardCall(nil), f.calls...)
}

// Forward implements Forwarder.
func (f *LocalForwarder) Forward(method, path, query string, headers map[string]string, body []byte, credential *Credential) (UpstreamResponse, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return f.ForwardStream(method, path, query, headers, reader, int64(len(body)), credential)
}

// ForwardStream implements StreamForwarder: path is
// /<owner>/<name>.git/<action>, as GitProxy forwards it.
func (f *LocalForwarder) ForwardStream(method, path, query string, headers map[string]string, body io.Reader, contentLength int64, credential *Credential) (UpstreamResponse, error) {
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return UpstreamResponse{}, fmt.Errorf("local upstream: bad path %q", path)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, ForwardCall{Method: method, Path: path, Query: query, Headers: headers, Credential: credential})
	f.mu.Unlock()
	cmd := exec.Command("git", "http-backend")
	cmd.Env = append(cmd.Environ(),
		"GIT_PROJECT_ROOT="+f.Root,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO="+path,
		"REQUEST_METHOD="+method,
		"QUERY_STRING="+query,
		"CONTENT_TYPE="+headers["Content-Type"],
		// git gzips a large upload-pack request body (RealForwarder's
		// own comment on Content-Encoding); http-backend only inflates
		// it when this says so.
		"HTTP_CONTENT_ENCODING="+headers["Content-Encoding"],
		"CONTENT_LENGTH="+strconv.FormatInt(contentLength, 10),
		// Set, so http-backend enables receive-pack.
		"REMOTE_USER=gitproxy",
	)
	cmd.Env = append(cmd.Env, receiveHardeningEnv()...)
	if body != nil {
		cmd.Stdin = body
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return UpstreamResponse{}, fmt.Errorf("git http-backend: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return UpstreamResponse{}, fmt.Errorf("git http-backend: %w", err)
	}
	stream := &cgiStream{cmd: cmd, r: bufio.NewReader(stdout)}
	h, err := textproto.NewReader(stream.r).ReadMIMEHeader()
	if err != nil {
		// Output that ends before the headers do is most often
		// http-backend refusing the request outright and exiting, and
		// its exit status is the more useful of the two errors. Anything
		// else is output that is not CGI headers at all, from a process
		// that may still be running.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if waitErr := stream.wait(); waitErr != nil {
				return UpstreamResponse{}, fmt.Errorf("git http-backend: %w", waitErr)
			}
		} else {
			stream.abort()
		}
		return UpstreamResponse{}, fmt.Errorf("reading git http-backend headers: %w", err)
	}
	status := http.StatusOK
	if value := h.Get("Status"); value != "" {
		status, err = strconv.Atoi(strings.Fields(value)[0])
		if err != nil {
			stream.abort()
			return UpstreamResponse{}, fmt.Errorf("reading git http-backend status: %w", err)
		}
		h.Del("Status")
	}
	headersOut := make(map[string]string, len(h))
	for key := range h {
		headersOut[key] = h.Get(key)
	}
	return UpstreamResponse{Status: status, Headers: headersOut, Stream: stream}, nil
}

// cgiStream is the body of git http-backend's response: the rest of its
// stdout once the CGI headers have been read off it. Reaching the end
// reaps the process, and a non-zero exit there is reported as the read's
// error rather than as io.EOF, so a response that ends because
// http-backend failed is not taken for a complete one. Closing it before
// the end -- the client went away mid-clone -- kills the process instead
// of leaving it writing to a pipe nobody reads.
type cgiStream struct {
	cmd     *exec.Cmd
	r       *bufio.Reader
	once    sync.Once
	waitErr error
	ended   bool
}

func (s *cgiStream) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err == io.EOF {
		s.ended = true
		if waitErr := s.wait(); waitErr != nil {
			return n, fmt.Errorf("git http-backend: %w", waitErr)
		}
	}
	return n, err
}

func (s *cgiStream) Close() error {
	if !s.ended {
		s.abort()
	}
	return nil
}

// abort kills the process, if it is still running, and reaps it,
// returning what Wait said.
func (s *cgiStream) abort() error {
	_ = s.cmd.Process.Kill()
	return s.wait()
}

func (s *cgiStream) wait() error {
	s.once.Do(func() { s.waitErr = s.cmd.Wait() })
	return s.waitErr
}
