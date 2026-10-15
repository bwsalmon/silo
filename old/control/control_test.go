package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T) *Server {
	t.Helper()
	// Unix socket paths are short; t.TempDir's can be too long.
	dir, err := os.MkdirTemp("", "ctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := Listen(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCalls(t *testing.T) {
	s := listen(t)
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket mode %v, want a 0600 socket", fi.Mode())
	}
	release := make(chan struct{})
	s.Handle("echo", func(ctx context.Context, p json.RawMessage) (any, error) {
		var v map[string]string
		if err := json.Unmarshal(p, &v); err != nil {
			return nil, Errorf("echo: %v", err)
		}
		return v, nil
	})
	s.Handle("fail", func(ctx context.Context, p json.RawMessage) (any, error) {
		return nil, errors.New("it broke")
	})
	s.Handle("slow", func(ctx context.Context, p json.RawMessage) (any, error) {
		<-release
		return "done", nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var got map[string]string
	if err := Call(ctx, s.Path(), "echo", map[string]string{"a": "b"}, &got); err != nil || got["a"] != "b" {
		t.Errorf("echo: %v %v", got, err)
	}
	var e *Error
	if err := Call(ctx, s.Path(), "echo", []int{1}, nil); !errors.As(err, &e) || e.Code != CodeInvalidParams {
		t.Errorf("bad params: %v", err)
	}
	if err := Call(ctx, s.Path(), "fail", nil, nil); !errors.As(err, &e) || e.Code != CodeInternalError || e.Message != "it broke" {
		t.Errorf("fail: %v", err)
	}
	if err := Call(ctx, s.Path(), "inject", nil, nil); !errors.As(err, &e) || e.Code != CodeMethodNotFound || !strings.Contains(e.Message, "echo") {
		t.Errorf("unknown method: %v", err)
	}

	// A slow call does not hold up a quick one on the same connection.
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"echo","params":{"x":"y"}}` + "\n" + "not json\n"))
	r := bufio.NewScanner(c)
	var lines []string
	for len(lines) < 2 && r.Scan() {
		lines = append(lines, r.Text())
	}
	close(release)
	r.Scan()
	lines = append(lines, r.Text())
	if !strings.Contains(lines[0]+lines[1], `"id":2,"result":{"x":"y"}`) || !strings.Contains(lines[0]+lines[1], `"code":-32700`) ||
		!strings.Contains(lines[2], `"id":1,"result":"done"`) {
		t.Errorf("answers %q", lines)
	}

	// A call ends with its context.
	short, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	release2 := make(chan struct{})
	defer close(release2)
	s.Handle("hang", func(ctx context.Context, p json.RawMessage) (any, error) {
		select {
		case <-release2:
		case <-ctx.Done():
		}
		return nil, nil
	})
	if err := Call(short, s.Path(), "hang", nil, nil); err == nil {
		t.Error("a hung call returned no error")
	}
}

func TestCloseRemovesTheSocket(t *testing.T) {
	s := listen(t)
	s.Handle("hang", func(ctx context.Context, p json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	errc := make(chan error, 1)
	go func() { errc <- Call(context.Background(), s.Path(), "hang", nil, nil) }()
	time.Sleep(100 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Error("a call cut off by Close returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the open call")
	}
	if err := Call(context.Background(), s.Path(), "hang", nil, nil); err == nil {
		t.Error("called a closed socket")
	}
}
