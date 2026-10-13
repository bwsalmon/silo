package main

import "testing"

func TestDockerEnv(t *testing.T) {
	d := startSilod(t)
	out := d.ok(d.root, "docker-env", "/", "--socket-dir", "/srv/docker")
	contains(t, "docker-env", out, "export DOCKER_HOST=unix:///srv/docker/"+d.root+".sock")
	var env map[string]string
	d.json(d.root, &env, "docker-env", "--socket-dir", "/srv/docker")
	if env["DOCKER_HOST"] != "unix:///srv/docker/"+d.root+".sock" || env["silo"] != d.root {
		t.Errorf("docker-env --output json: %v", env)
	}
}
