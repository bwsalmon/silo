package compose_test

import (
	"strings"
	"testing"
)

// tcLabels are the labels testcontainers puts on what it makes.
func tcLabels() obj {
	return obj{"org.testcontainers": "true", "org.testcontainers.lang": "go",
		"org.testcontainers.version": "0.34.0", "org.testcontainers.sessionId": "4f6b1c2e"}
}

// testcontainers with the test process inside the silo and
// DOCKER_HOST=tcp://docker.silo.internal:2375 (§5): /_ping, /version
// and /info; pull; create with labels and a mapped port; a file copied
// in before start; start; inspect for the mapped port; the log and port
// wait strategies; connecting to docker.silo.internal:<mapped port>;
// exec; stop and remove.
func TestTestcontainersGeneric(t *testing.T) {
	eachClass(t, func(t *testing.T, e env) {
		for _, p := range []string{"/_ping", "/version", "/info"} {
			if r := e.DockerDo(e.silo, "GET", "/v1.47"+p, nil); r.Code != 200 {
				t.Fatalf("%s: %d %s", p, r.Code, r.Body)
			}
		}
		// The test process itself, a container of the silo's.
		runner := e.create("go-test", obj{"Image": "golang:1.23", "Cmd": script("exec sleep infinity")})
		e.do("POST", "/containers/"+runner+"/start", nil, nil)

		id := e.create("tc-redis", obj{
			"Image":        "redis:7",
			"Cmd":          script("echo Ready to accept connections tcp; exec sleep infinity"),
			"Labels":       tcLabels(),
			"ExposedPorts": obj{"6379/tcp": obj{}},
			"HostConfig":   obj{"PortBindings": obj{"6379/tcp": []obj{{"HostIp": "", "HostPort": ""}}}},
		})
		// copyFileToContainer before start.
		const conf = "maxmemory 64mb\n"
		e.putArchive(id, "/", tarOf(map[string]string{"etc/redis.conf": conf}))
		e.do("POST", "/containers/"+id+"/start", nil, nil)

		// The mapped port, from inspect.
		bound := e.inspect(id).NetworkSettings.Ports["6379/tcp"]
		if len(bound) == 0 || bound[0].HostPort == "" {
			t.Fatalf("no mapped port for 6379/tcp: %v", e.inspect(id).NetworkSettings.Ports)
		}
		mapped := bound[0].HostPort

		// Log wait strategy.
		e.waitLog(id, "Ready to accept connections")
		// Port wait strategy: a shell check inside the container with
		// exec, then a connection from outside it.
		if code, _, errOut := e.exec(id, "/bin/sh", "-c", "true && nc -z -w 1 localhost 6379"); code != 0 {
			t.Errorf("port wait's internal check: exit %d, %s", code, errOut)
		}
		if code, _, errOut := e.exec(runner, "sh", "-c", "nc -z -w 2 docker.silo.internal "+mapped); code != 0 {
			t.Errorf("the test process reaching docker.silo.internal:%s: exit %d, %s", mapped, code, errOut)
		}
		// The copied file is there.
		if code, out, errOut := e.exec(id, "cat", "/etc/redis.conf"); code != 0 || out != conf {
			t.Errorf("cat the copied file: exit %d, %q %q", code, out, errOut)
		}

		// Terminate: stop, then remove with its volumes.
		e.do("POST", "/containers/"+id+"/stop?t=10", nil, nil)
		e.do("DELETE", "/containers/"+id+"?force=1&v=1", nil, nil)
		if left := e.listed("org.testcontainers.sessionId=4f6b1c2e"); len(left) != 0 {
			t.Errorf("after terminate: %v", left)
		}
	})
}

// Ryuk, the reaper testcontainers starts, bind-mounts the Docker
// socket, and that bind is refused (§5), so in a silo testcontainers
// runs with TESTCONTAINERS_RYUK_DISABLED=true and the silo is the
// reaper. Docker inside a container, which needs the socket or
// privileges, is refused in both classes too.
func TestTestcontainersRyukRefused(t *testing.T) {
	eachClass(t, func(t *testing.T, e env) {
		e.pull("testcontainers/ryuk:0.10.2")
		for name, body := range map[string]obj{
			"ryuk": {"Image": "testcontainers/ryuk:0.10.2", "Labels": obj{"org.testcontainers.ryuk": "true"},
				"HostConfig": obj{"Binds": []string{"/var/run/docker.sock:/var/run/docker.sock"}}},
			"socket as a mount": {"Image": "testcontainers/ryuk:0.10.2",
				"HostConfig": obj{"Mounts": []obj{{"Type": "bind", "Source": "/var/run/docker.sock", "Target": "/var/run/docker.sock"}}}},
			"docker in docker": {"Image": "testcontainers/ryuk:0.10.2", "HostConfig": obj{"Privileged": true}},
		} {
			body["HostConfig"].(obj)["Runtime"] = e.class
			if r := e.DockerDo(e.silo, "POST", "/v1.47/containers/create?name=reaper", body); r.Code != 403 {
				t.Errorf("%s: %d %s, want 403", name, r.Code, r.Body)
			}
		}
	})
}

// The modules §5 says the sandbox class must pass, and the vm class
// once plan item 9 lands: Postgres, MySQL, Redis and Kafka, each started
// as its module starts it, waited for as its module waits, and asked
// whether it answers with its own client by exec. They need the images
// themselves, so a target whose containers don't run them skips.
func TestTestcontainersModules(t *testing.T) {
	modules := []struct {
		name, image string
		env         []string
		ready       string
		check       []string
	}{
		{"postgres", "postgres:16-alpine", []string{"POSTGRES_PASSWORD=test", "POSTGRES_DB=test"},
			"database system is ready to accept connections", []string{"pg_isready", "-U", "postgres"}},
		{"mysql", "mysql:8.4", []string{"MYSQL_ROOT_PASSWORD=test", "MYSQL_DATABASE=test"},
			"ready for connections", []string{"mysqladmin", "ping", "-uroot", "-ptest"}},
		{"redis", "redis:7-alpine", nil, "Ready to accept connections", []string{"redis-cli", "ping"}},
		{"kafka", "apache/kafka:3.8.0", nil, "Kafka Server started",
			[]string{"/opt/kafka/bin/kafka-topics.sh", "--bootstrap-server", "localhost:9092", "--list"}},
	}
	eachClass(t, func(t *testing.T, e env) {
		e.simulated(t)
		for _, m := range modules {
			t.Run(m.name, func(t *testing.T) {
				e := e
				e.T = t
				id := e.create("tc-"+m.name, obj{"Image": m.image, "Env": m.env, "Labels": tcLabels()})
				t.Cleanup(func() { e.DockerDo(e.silo, "DELETE", "/v1.47/containers/"+id+"?force=1&v=1", nil) })
				e.do("POST", "/containers/"+id+"/start", nil, nil)
				e.waitLog(id, m.ready)
				if code, out, errOut := e.exec(id, m.check...); code != 0 {
					t.Errorf("%s: exit %d, %s %s", strings.Join(m.check, " "), code, out, errOut)
				}
			})
		}
	})
}
