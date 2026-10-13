package dockergw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/pkg/facade/local/docker"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/frontdoor"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

func jsonMessage(msg string) ([]byte, error) { return json.Marshal(map[string]string{"message": msg}) }

// Sockets are the host's Docker sockets, <Dir>/<silo id>.sock, one per
// silo: /run/silo/docker, or $XDG_RUNTIME_DIR/silo/docker for a
// rootless install without /run/silo (install's DockerSocket). Each is
// mode 0666, so that any local user can connect, in a directory silod's
// own user owns: authority is the peer uid's member silo, read with
// SO_PEERCRED per request, not the file's mode. A uid mapped to no silo
// gets 403 on everything but /_ping.
type Sockets struct {
	Dir   string
	Store store.Store
	// Handler is the facade.
	Handler http.Handler
	// PeerUID is the uid of a connection's peer; SO_PEERCRED if nil.
	PeerUID func(net.Conn) (uint32, error)
	// Logf logs a socket that couldn't be opened; log.Printf if nil.
	Logf func(string, ...any)

	mu      sync.Mutex
	servers map[string]*http.Server
}

// Path is silo's socket.
func (s *Sockets) Path(silo string) string { return filepath.Join(s.Dir, silo+".sock") }

// prepare makes Dir, and refuses one another user owns: whoever owns
// the directory can put their own socket in a silo's place.
func (s *Sockets) prepare() error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	fi, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("dockergw: %s is not a directory", s.Dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("dockergw: %s belongs to uid %d, not silod's %d", s.Dir, st.Uid, os.Getuid())
	}
	return os.Chmod(s.Dir, 0o755)
}

// Sync opens a socket for every silo that has none, and closes and
// removes those of silos that are gone.
func (s *Sockets) Sync(ctx context.Context) error {
	var silos []*tree.Silo
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		silos, err = tree.All(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.servers == nil {
		if err := s.prepare(); err != nil {
			return err
		}
		s.servers = map[string]*http.Server{}
	}
	live := map[string]bool{}
	var errs []error
	for _, t := range silos {
		live[t.ID] = true
		if s.servers[t.ID] != nil {
			continue
		}
		srv, err := s.open(t.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.servers[t.ID] = srv
	}
	for id, srv := range s.servers {
		if !live[id] {
			srv.Close()
			os.Remove(s.Path(id))
			delete(s.servers, id)
		}
	}
	return errors.Join(errs...)
}

func (s *Sockets) open(silo string) (*http.Server, error) {
	path := s.Path(silo)
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	srv := &http.Server{Handler: s.handler(silo), ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uid, err := s.peerUID(c)
			if err != nil {
				return ctx
			}
			return context.WithValue(ctx, uidKey{}, uid)
		}}
	go srv.Serve(l)
	return srv, nil
}

func (s *Sockets) peerUID(c net.Conn) (uint32, error) {
	if s.PeerUID != nil {
		return s.PeerUID(c)
	}
	return PeerCred(c)
}

type uidKey struct{}

// handler serves silo's socket: its target is silo, and its caller the
// peer uid's member silo, read again per request so that moving the
// uid takes effect on an open connection.
func (s *Sockets) handler(silo string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stripSiloHeaders(r)
		ctx := docker.ContextWithSocket(r.Context(), silo)
		uid, ok := r.Context().Value(uidKey{}).(uint32)
		var caller fenceapi.Caller
		if ok {
			id := fenceapi.UIDIdentity(uid)
			member, err := frontdoor.MemberSilo(ctx, s.Store, id)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				b, _ := jsonMessage("silo: reading the member table: " + err.Error())
				w.Write(b)
				return
			}
			caller = fenceapi.Caller{Silo: member, Via: fenceapi.ViaFrontDoor, Member: id}
		}
		if caller.Silo == "" {
			if IsPing(r) {
				s.Handler.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			who := "the connection's peer uid is unknown"
			if ok {
				who = fmt.Sprintf("%s is not a member of any silo", fenceapi.UIDIdentity(uid))
			}
			refuse(w, "silo: "+who)
			return
		}
		s.Handler.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(ctx, caller)))
	})
}

// Run opens the sockets, and follows the tree every interval until ctx
// ends, when it closes them all.
func (s *Sockets) Run(ctx context.Context, interval time.Duration) {
	logf := s.Logf
	if logf == nil {
		logf = log.Printf
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			logf("docker sockets: %v", err)
		}
		select {
		case <-ctx.Done():
			s.Close()
			return
		case <-t.C:
		}
	}
}

// Close closes every socket and removes its file.
func (s *Sockets) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, srv := range s.servers {
		srv.Close()
		os.Remove(s.Path(id))
	}
	s.servers = nil
}
