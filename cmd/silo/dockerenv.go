package main

import (
	"context"
	"fmt"
	"os"

	localinstall "github.com/bwsalmon/silo/pkg/install/local"
)

// dockerEnv prints DOCKER_HOST for a silo's Docker socket on the local
// backend (local-backend.md §4, "Address"): /run/silo/docker/<id>.sock,
// or under $XDG_RUNTIME_DIR/silo/docker where there is no /run/silo,
// as install's context names the root's.
func (c *cli) dockerEnv(ctx context.Context, args []string) error {
	fs := c.flags("docker-env")
	dir := fs.String("socket-dir", "", "the directory silod serves the Docker sockets in (default /run/silo/docker, or $XDG_RUNTIME_DIR/silo/docker without /run/silo)")
	pos, err := c.parse(fs, args, 0, 1, "[silo]")
	if err != nil {
		return err
	}
	s, err := c.api.Describe(ctx, arg(pos, 0, "self"), false)
	if err != nil {
		return err
	}
	path := ""
	if *dir != "" {
		path = fmt.Sprintf("%s/%s.sock", *dir, s.ID)
	} else {
		fi, err := os.Stat("/run/silo")
		path = localinstall.DockerSocket(err == nil && fi.IsDir(), c.getenv("XDG_RUNTIME_DIR"), s.ID)
	}
	host := "unix://" + path
	return c.print(map[string]any{"DOCKER_HOST": host, "silo": s.ID, "path": s.Path}, func(w *tw) {
		fmt.Fprintf(w, "export DOCKER_HOST=%s\n", host)
		fmt.Fprintf(w, "# Run this command to configure your shell: eval \"$(silo docker-env %s)\"\n", s.Path)
	})
}
