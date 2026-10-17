package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/server"
	"github.com/bwsalmon/silo/hub/tasks"
)

// The kit's hub signs humans in by a dev header: these are its people.
const (
	userHeader = "X-Silo-Conformance-User"
	admin      = "admin@conformance.invalid"
	// operator owns the Grain under test and submits every task.
	operator = "operator@conformance.invalid"
	// reviewer, a second member of the team, answers approvals.
	reviewer = "reviewer@conformance.invalid"
	team     = "conformance"
)

// hub is the kit's own hub, run in this process, which it can stop and
// start again on the same address and database.
type hub struct {
	addr, url string
	cfg       server.Config
	logf      func(string, ...any)

	stop context.CancelFunc
	done chan error
	srv  *server.Hub
}

func startHub(dir, addr string, heartbeat time.Duration, logf func(string, ...any)) (*hub, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	h := &hub{
		addr: l.Addr().String(),
		logf: logf,
		cfg: server.Config{
			Database:      "sqlite:" + filepath.Join(dir, "hub.db"),
			DevUserHeader: userHeader,
			HubAdmins:     []string{"user:" + admin},
			Heartbeat:     heartbeat,
			DispatchRetry: time.Second,
			ShutdownGrace: 5 * time.Second,
			Logf:          logf,
		},
	}
	h.url = "http://" + h.addr
	if err := h.serve(l); err != nil {
		return nil, err
	}
	return h, h.healthy(30 * time.Second)
}

func (h *hub) serve(l net.Listener) error {
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := server.New(ctx, h.cfg)
	if err != nil {
		cancel()
		l.Close()
		return err
	}
	h.srv, h.stop, h.done = srv, cancel, make(chan error, 1)
	go func() {
		err := srv.Serve(ctx, l)
		srv.Close()
		h.done <- err
	}()
	return nil
}

func (h *hub) healthy(d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(h.url + server.HealthzPath); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case err := <-h.done:
			h.done <- err
			return fmt.Errorf("the hub stopped: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("the hub did not answer %s within %s", server.HealthzPath, d)
}

// Close shuts the hub down as a rolled pod is: its nodes are sent
// goodbye.
func (h *hub) Close() error {
	if h.stop == nil {
		return nil
	}
	h.stop()
	h.stop, h.srv = nil, nil
	select {
	case err := <-h.done:
		if errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-time.After(30 * time.Second):
		return errors.New("the hub did not stop within 30s")
	}
}

// Restart stops the hub and starts it again on the same address and
// database.
func (h *hub) Restart() error {
	if err := h.Close(); err != nil {
		return err
	}
	var l net.Listener
	var err error
	for range 100 {
		if l, err = net.Listen("tcp", h.addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if err := h.serve(l); err != nil {
		return err
	}
	return h.healthy(30 * time.Second)
}

// open opens a stream to node as the hub does, past every check the
// hub makes first: for asking the Grain itself what the hub would not.
func (h *hub) open(ctx context.Context, node string) (net.Conn, error) {
	if h.srv == nil {
		return nil, errors.New("the hub is not running")
	}
	return h.srv.Rendezvous.Open(ctx, node)
}

var client = &http.Client{Timeout: 30 * time.Second}

// call makes an API request as user, decoding a JSON answer into out
// when it is set. An answer outside 2xx is an error.
func (h *hub) call(ctx context.Context, method, path, user string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.url+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set(userHeader, user)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// setup makes the team that owns the kit's repositories, with the
// operator and the reviewer in it, and mints node's token.
func (h *hub) setup(ctx context.Context, node string, repos []string) (string, error) {
	steps := []func() error{
		func() error {
			return h.call(ctx, "POST", "/v1/teams", admin, map[string]string{"id": team, "name": "Conformance"}, nil)
		},
		func() error {
			return h.call(ctx, "PUT", "/v1/teams/"+team+"/members/user:"+operator, admin, map[string]string{"role": "member"}, nil)
		},
		func() error {
			return h.call(ctx, "PUT", "/v1/teams/"+team+"/members/user:"+reviewer, admin, map[string]string{"role": "member"}, nil)
		},
	}
	for _, r := range repos {
		steps = append(steps, func() error { return h.call(ctx, "PUT", "/v1/teams/"+team+"/repos/"+r, admin, nil, nil) })
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return "", err
		}
	}
	var minted struct {
		Token string `json:"token"`
	}
	err := h.call(ctx, "POST", "/v1/node-tokens", operator, map[string]string{"node": node}, &minted)
	return minted.Token, err
}

// node is the directory's entry for id, if it has one.
func (h *hub) node(ctx context.Context, id string) (registry.Entry, bool, error) {
	var dir struct {
		Nodes []registry.Entry `json:"nodes"`
	}
	if err := h.call(ctx, "GET", rendezvous.DirectoryPath, operator, nil, &dir); err != nil {
		return registry.Entry{}, false, err
	}
	for _, e := range dir.Nodes {
		if e.ID == id {
			return e, true, nil
		}
	}
	return registry.Entry{}, false, nil
}

// submit files a conformance task in repo.
func (h *hub) submit(ctx context.Context, repo, kind, title, prompt string) (tasks.Task, error) {
	var t tasks.Task
	body := map[string]any{"repo": repo, "title": title, "prompt": prompt}
	if kind != "" {
		body["kind"] = kind
	}
	err := h.call(ctx, "POST", tasks.TasksPath, operator, body, &t)
	return t, err
}

func (h *hub) task(ctx context.Context, id string) (tasks.Task, error) {
	var t tasks.Task
	err := h.call(ctx, "GET", tasks.TasksPath+"/"+id, operator, nil, &t)
	return t, err
}

func (h *hub) cancel(ctx context.Context, id string) error {
	return h.call(ctx, "POST", tasks.TasksPath+"/"+id+"/cancel", operator, map[string]string{"reason": "conformance"}, nil)
}

// view reads a task's artifact through the hub's view-through.
func (h *hub) view(ctx context.Context, id, ref string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", h.url+tasks.TasksPath+"/"+id+"/artifacts?ref="+url.QueryEscape(ref), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(userHeader, operator)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, err
}

// events reads task id's events after seq from, as the hub serves a
// subscriber (Server-Sent Events), until the task's events end or ctx
// does, handing each to each in order.
func (h *hub) events(ctx context.Context, id string, from int64, each func(live.Event)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s%s/%s/events?from=%d", h.url, tasks.TasksPath, id, from), nil)
	if err != nil {
		return err
	}
	req.Header.Set(userHeader, operator)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 2<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var e live.Event
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &e) != nil {
			continue
		}
		if e.Type == "end" {
			return nil
		}
		each(e)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}
