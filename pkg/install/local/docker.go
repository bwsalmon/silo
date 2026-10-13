package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/bwsalmon/silo/pkg/backend/local/units"
	"github.com/bwsalmon/silo/pkg/install"
)

// DockerContext is the docker context install writes for the root.
const DockerContext = "silo-root"

// DockerSocket is silo's Docker socket for the silo id: under
// /run/silo/docker, which every local user can reach, where /run/silo
// exists (the system install's, or the rootless tmpfiles entry), and
// under the installer's $XDG_RUNTIME_DIR/silo/docker otherwise
// (local-backend.md §4, "Address").
func DockerSocket(runSilo bool, xdgRuntimeDir, id string) string {
	dir := "/run/silo"
	if !runSilo {
		dir = filepath.Join(xdgRuntimeDir, "silo")
	}
	return filepath.Join(dir, "docker", id+".sock")
}

// dockerStep writes a docker context for the root silo into the
// installing user's docker configuration (call L23, step 5) and prints
// how to use it. It doesn't make it the current context.
type dockerStep struct {
	c Config
	h Host
}

type dockerObserved struct {
	RootID string `json:"rootID,omitempty"`
	// Config is the docker configuration directory, and Owner the user
	// it belongs to, when that isn't the installer (sudo).
	Config string `json:"config"`
	Owner  string `json:"owner,omitempty"`
	// Have is the context's meta.json, if there is one.
	Have    string `json:"have,omitempty"`
	RunSilo bool   `json:"runSilo"`
	Error   string `json:"error,omitempty"`
}

func (s *dockerStep) Name() string                  { return "docker context" }
func (s *dockerStep) NewObserved() install.Observed { return &dockerObserved{} }

// metaPath is where docker keeps a context's metadata: under
// contexts/meta/, in a directory named by the SHA-256 of its name.
func metaPath(config string) string {
	sum := sha256.Sum256([]byte(DockerContext))
	return filepath.Join(config, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
}

func (s *dockerStep) Observe(ctx context.Context) (install.Observed, error) {
	o := &dockerObserved{}
	if h, _ := health(ctx, s.c, s.h); h != nil {
		o.RootID = h.RootID
	}
	_, err := s.h.Stat("/run/silo")
	o.RunSilo = err == nil || s.c.Mode == units.System
	o.Config, o.Owner, err = s.dockerConfig()
	if err != nil {
		o.Error = err.Error()
		return o, nil
	}
	if b, err := s.h.ReadFile(metaPath(o.Config)); err == nil {
		o.Have = string(b)
	}
	return o, nil
}

// dockerConfig is the installing user's docker configuration directory:
// $DOCKER_CONFIG, or ~/.docker; under sudo, the invoking user's.
func (s *dockerStep) dockerConfig() (dir, owner string, err error) {
	name := s.c.User
	if s.h.EUID == 0 {
		name = "root"
		if u := s.h.Env("SUDO_USER"); u != "" {
			name, owner = u, u
		}
	}
	if d := s.h.Env("DOCKER_CONFIG"); owner == "" && filepath.IsAbs(d) {
		return d, "", nil
	}
	u, err := s.h.Lookup(name)
	if err != nil {
		return "", "", err
	}
	if u.HomeDir == "" {
		return "", "", errors.New(name + " has no home directory")
	}
	return filepath.Join(u.HomeDir, ".docker"), owner, nil
}

// meta is the context's meta.json for the root's socket.
func meta(socket string) string {
	b, _ := json.Marshal(map[string]any{
		"Name":     DockerContext,
		"Metadata": map[string]string{"Description": "Silo's root silo, local backend (written by silo install)"},
		"Endpoints": map[string]any{
			"docker": map[string]any{"Host": "unix://" + socket, "SkipTLSVerify": false},
		},
	})
	return string(b) + "\n"
}

func (s *dockerStep) socket(o *dockerObserved) string {
	return DockerSocket(o.RunSilo, runtimeDir(s.h), o.RootID)
}

func (s *dockerStep) Plan(obs install.Observed) []install.Change {
	o := obs.(*dockerObserved)
	if o.Error != "" {
		return []install.Change{{Action: install.ActionWarn, Object: DockerContext, Detail: "no docker configuration to write it to: " + o.Error,
			Why: "docker --context " + DockerContext}}
	}
	path := metaPath(o.Config)
	if o.RootID == "" {
		return []install.Change{{Action: install.ActionCreate, Object: DockerContext,
			Detail: fmt.Sprintf("%s: DOCKER_HOST unix://%s, once the root exists", path, DockerSocket(o.RunSilo, runtimeDir(s.h), "<root id>"))}}
	}
	want := meta(s.socket(o))
	switch {
	case o.Have == want:
		return []install.Change{{Action: install.ActionOK, Object: DockerContext, Detail: "unix://" + s.socket(o)}}
	case o.Have == "":
		return []install.Change{{Action: install.ActionCreate, Object: DockerContext, Detail: path + ": unix://" + s.socket(o)}}
	}
	return []install.Change{{Action: install.ActionUpdate, Object: DockerContext, Detail: path + ": unix://" + s.socket(o)}}
}

func (s *dockerStep) PlanRemove(obs install.Observed) []install.Change {
	o := obs.(*dockerObserved)
	if o.Have == "" {
		return []install.Change{{Action: install.ActionOK, Object: DockerContext, Detail: "not there"}}
	}
	return []install.Change{{Action: install.ActionDelete, Object: DockerContext, Detail: filepath.Dir(metaPath(o.Config))}}
}

func (s *dockerStep) Apply(ctx context.Context, changes []install.Change) error {
	obs, err := s.Observe(ctx)
	if err != nil {
		return err
	}
	o := obs.(*dockerObserved)
	for _, ch := range changes {
		switch ch.Action {
		case install.ActionDelete:
			if err := s.h.RemoveAll(filepath.Dir(metaPath(o.Config))); err != nil {
				return err
			}
			fmt.Fprintf(s.h.Progress, "  removed docker context %s\n", DockerContext)
		case install.ActionCreate, install.ActionUpdate:
			if o.RootID == "" {
				return errors.New("silod has no root silo to write a docker context for")
			}
			if err := s.h.WriteFile(metaPath(o.Config), []byte(meta(s.socket(o)))); err != nil {
				return err
			}
			if o.Owner != "" {
				// Written by root into the sudo user's configuration.
				if _, err := s.h.Run(ctx, "chown", "-R", o.Owner+":", filepath.Join(o.Config, "contexts")); err != nil {
					return err
				}
			}
			fmt.Fprintf(s.h.Progress, "  wrote docker context %s\n\nTo use the root silo from docker:\n  docker context use %s\nor\n  export DOCKER_HOST=unix://%s\n",
				DockerContext, DockerContext, s.socket(o))
		}
	}
	return nil
}
