package targets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// RequireLocalEnv, set, makes a local install that can't be found a
// failure of the cases that need one rather than a skip: the
// local-conformance job sets it after `silo install --backend local`.
const RequireLocalEnv = "SILO_REQUIRE_LOCAL_TARGETS"

// Install is the local backend as `silo install --backend local` left
// it on this host: silod's front door, where its peer uid is the root's
// admin, and the root silo's Docker facade socket.
type Install struct {
	// Socket is silod's front door, silo.sock.
	Socket string
	// Root is the root silo's id.
	Root string
	// Docker is the root's Docker facade socket.
	Docker string
}

// FindInstall finds the local install the current user's silod serves:
// /run/silo when it has one, else $XDG_RUNTIME_DIR/silo, as install's
// units put it (units.Socket, localinstall.DockerSocket).
func FindInstall(ctx context.Context) (*Install, error) {
	var dirs []string
	if fi, err := os.Stat("/run/silo/silo.sock"); err == nil && fi.Mode()&os.ModeSocket != 0 {
		dirs = append(dirs, "/run/silo")
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		dirs = append(dirs, filepath.Join(x, "silo"))
	}
	if len(dirs) == 0 {
		return nil, errors.New("no /run/silo/silo.sock, and no XDG_RUNTIME_DIR to look in")
	}
	var errs []error
	for _, d := range dirs {
		in := &Install{Socket: filepath.Join(d, "silo.sock")}
		var health struct {
			Status string `json:"status"`
			RootID string `json:"root_id"`
		}
		if err := getJSON(ctx, UnixClient(in.Socket), "http://silo/silod/health", &health); err != nil {
			errs = append(errs, err)
			continue
		}
		if health.RootID == "" {
			errs = append(errs, fmt.Errorf("%s: silod has no root silo (%s)", in.Socket, health.Status))
			continue
		}
		in.Root = health.RootID
		in.Docker = filepath.Join(d, "docker", in.Root+".sock")
		// silod serves a silo's socket once it has seen the silo, which
		// for a root install has just made is a moment after.
		for {
			if fi, err := os.Stat(in.Docker); err == nil && fi.Mode()&os.ModeSocket != 0 {
				return in, nil
			}
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%s: the root's Docker socket %s never appeared", in.Socket, in.Docker)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return nil, fmt.Errorf("no installed silod answers: %w", errors.Join(errs...))
}

// UnixClient is an HTTP client whose every request goes to socket.
func UnixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}, Timeout: 5 * time.Minute}
}

func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
