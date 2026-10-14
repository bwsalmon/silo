// Package compose_test is local-backend.md §5's acceptance cases
// (call L11, §15 item 13): the Docker compose v2 and testcontainers
// flows, scripted as the Engine API calls those tools make, run in
// both isolation classes against every conformance.Target's Fence, and
// what gVisor won't run recorded as expected skips. local-conformance
// runs them on every push.
package compose_test

import (
	"strings"
	"testing"
	"time"
)

// project is a compose project's labels on everything it makes.
func project(name, service string) obj {
	l := obj{"com.docker.compose.project": name, "com.docker.compose.version": "2.29.7"}
	if service != "" {
		l["com.docker.compose.service"] = service
		l["com.docker.compose.container-number"] = "1"
	}
	return l
}

// service is the create body compose sends for service in project
// shop: on the project network under its service name, with extra
// fields merged in.
func service(name, image, cmd string, extra obj) obj {
	b := obj{
		"Image":  image,
		"Cmd":    script(cmd),
		"Labels": project("shop", name),
		"HostConfig": obj{
			"NetworkMode":   "shop_default",
			"RestartPolicy": obj{"Name": "no"},
		},
		"NetworkingConfig": obj{"EndpointsConfig": obj{
			"shop_default": obj{"Aliases": []string{"shop-" + name + "-1", name}},
		}},
	}
	for k, v := range extra {
		if k == "HostConfig" {
			for hk, hv := range v.(obj) {
				b["HostConfig"].(obj)[hk] = hv
			}
			continue
		}
		b[k] = v
	}
	return b
}

// compose up -d of a web app, a database and a cache on the project
// network, the database's data on a named volume and the web app's
// port published; then ps, the services finding each other by name,
// exec, logs, top, the published port from inside the silo at
// docker.silo.internal, stop, start, restart, run and down -v.
func TestComposeProject(t *testing.T) {
	eachClass(t, func(t *testing.T, e env) {
		// up -d: the network and volume, then each service's
		// container, created and started in depends_on order.
		e.do("POST", "/networks/create", obj{"Name": "shop_default", "Driver": "bridge",
			"Labels": obj{"com.docker.compose.project": "shop", "com.docker.compose.network": "default"}}, nil)
		e.do("POST", "/volumes/create", obj{"Name": "shop_dbdata",
			"Labels": obj{"com.docker.compose.project": "shop", "com.docker.compose.volume": "dbdata"}}, nil)
		db := e.create("shop-db-1", service("db", "postgres:16", "echo database system is ready to accept connections; exec sleep infinity",
			obj{"HostConfig": obj{"Mounts": []obj{{"Type": "volume", "Source": "shop_dbdata", "Target": "/var/lib/postgresql/data"}}}}))
		cache := e.create("shop-cache-1", service("cache", "redis:7", "echo Ready to accept connections tcp; exec sleep infinity", nil))
		web := e.create("shop-web-1", service("web", "nginx:1", "echo web listening on 8080; exec sleep infinity", obj{
			"ExposedPorts": obj{"8080/tcp": obj{}},
			"HostConfig":   obj{"PortBindings": obj{"8080/tcp": []obj{{"HostIp": "", "HostPort": "8080"}}}},
		}))
		for _, id := range []string{db, cache, web} {
			e.do("POST", "/containers/"+id+"/start", nil, nil)
		}

		// ps
		ps := e.listed("com.docker.compose.project=shop")
		if len(ps) != 3 {
			t.Fatalf("ps: %d containers, want 3: %v", len(ps), ps)
		}
		for _, c := range ps {
			if c.State != "running" {
				t.Errorf("ps: %v is %s", c.Names, c.State)
			}
		}

		// Services find each other by service name and container name.
		for _, c := range []struct {
			host, port string
		}{{"db", "5432"}, {"cache", "6379"}, {"shop-db-1", "5432"}} {
			if code, out, errOut := e.exec(web, "sh", "-c", "nc -z -w 2 "+c.host+" "+c.port); code != 0 {
				t.Errorf("web reaching %s:%s: exit %d, %q %q", c.host, c.port, code, out, errOut)
			}
		}
		// A name no service has resolves to nothing.
		if code, _, _ := e.exec(web, "sh", "-c", "nc -z -w 2 nosuchservice 80"); code == 0 {
			t.Errorf("web reached nosuchservice")
		}

		// exec, with its output and exit code.
		if code, out, _ := e.exec(db, "sh", "-c", "echo from-exec"); code != 0 || strings.TrimSpace(out) != "from-exec" {
			t.Errorf("compose exec db: exit %d, %q", code, out)
		}
		if code, _, _ := e.exec(db, "sh", "-c", "exit 3"); code != 3 {
			t.Errorf("compose exec db exit 3: exit %d", code)
		}

		// logs, and logs -f.
		e.waitLog(db, "ready to accept connections")
		if ok, got := e.follow("GET", "/containers/"+cache+"/logs?follow=1&stdout=1&stderr=1", "Ready to accept connections"); !ok {
			t.Errorf("compose logs -f cache: %q", got)
		}

		// top
		e.do("GET", "/containers/"+web+"/top", nil, nil)

		// ports: web's 8080 at docker.silo.internal, from another of the
		// silo's containers.
		if in := e.inspect(web); len(in.NetworkSettings.Ports["8080/tcp"]) == 0 {
			t.Errorf("web's 8080/tcp isn't published: %v", in.NetworkSettings.Ports)
		}
		if code, _, errOut := e.exec(cache, "sh", "-c", "nc -z -w 2 docker.silo.internal 8080"); code != 0 {
			t.Errorf("cache reaching web's published port at docker.silo.internal: exit %d, %s", code, errOut)
		}

		// stop, start, restart.
		e.do("POST", "/containers/"+cache+"/stop?t=1", nil, nil)
		if in := e.inspect(cache); in.State.Running {
			t.Errorf("stopped cache is %s", in.State.Status)
		}
		e.do("POST", "/containers/"+cache+"/start", nil, nil)
		e.do("POST", "/containers/"+web+"/restart?t=1", nil, nil)
		for _, id := range []string{cache, web} {
			if in := e.inspect(id); !in.State.Running {
				t.Errorf("%s after start/restart is %s", in.Name, in.State.Status)
			}
		}

		// run: a one-off container of the web service, attached, then
		// removed.
		one := service("web", "nginx:1", "echo migrated; exit 0", nil)
		one["Labels"].(obj)["com.docker.compose.oneoff"] = "True"
		run := e.create("shop-web-run-1", one)
		e.do("POST", "/containers/"+run+"/start", nil, nil)
		var waited struct{ StatusCode int }
		e.do("POST", "/containers/"+run+"/wait", nil, &waited)
		if waited.StatusCode != 0 {
			t.Errorf("compose run web: exit %d", waited.StatusCode)
		}
		if out, _ := e.logs(run); !strings.Contains(out, "migrated") {
			t.Errorf("compose run web's output: %q", out)
		}
		e.do("DELETE", "/containers/"+run, nil, nil)

		// down -v: stop and remove the containers, then the network and
		// the volume. Nothing of the project is left.
		for _, id := range []string{web, cache, db} {
			e.do("POST", "/containers/"+id+"/stop?t=1", nil, nil)
			e.do("DELETE", "/containers/"+id, nil, nil)
		}
		e.do("DELETE", "/networks/shop_default", nil, nil)
		e.do("DELETE", "/volumes/shop_dbdata", nil, nil)
		if left := e.listed("com.docker.compose.project=shop"); len(left) != 0 {
			t.Errorf("after down -v: %v", left)
		}
		if r := e.DockerDo(e.silo, "GET", "/v1.47/networks/shop_default", nil); r.Code != 404 {
			t.Errorf("network after down -v: %d %s", r.Code, r.Body)
		}
		if r := e.DockerDo(e.silo, "GET", "/v1.47/volumes/shop_dbdata", nil); r.Code != 404 {
			t.Errorf("volume after down -v: %d %s", r.Code, r.Body)
		}
	})
}

// compose up in the foreground: compose attaches to each container
// before starting it, and shows what it writes.
func TestComposeAttach(t *testing.T) {
	eachClass(t, func(t *testing.T, e env) {
		e.do("POST", "/networks/create", obj{"Name": "shop_default", "Labels": project("shop", "")}, nil)
		id := e.create("shop-web-1", service("web", "nginx:1", "echo attached and serving; exec sleep infinity", nil))
		done := make(chan struct{})
		var ok bool
		var got string
		go func() {
			defer close(done)
			ok, got = e.follow("POST", "/containers/"+id+"/attach?stream=1&stdout=1&stderr=1&logs=1", "attached and serving")
		}()
		// compose starts the container once it has attached.
		time.Sleep(200 * time.Millisecond)
		e.do("POST", "/containers/"+id+"/start", nil, nil)
		<-done
		if !ok {
			t.Errorf("compose up (attached): %q", got)
		}
	})
}

// depends_on: condition: service_healthy. silod runs the HEALTHCHECK
// with the driver's exec and reports State.Health (§5): healthy once
// the test passes, and unhealthy once it has failed Retries times.
func TestComposeHealthcheck(t *testing.T) {
	eachClass(t, func(t *testing.T, e env) {
		e.do("POST", "/networks/create", obj{"Name": "shop_default", "Labels": project("shop", "")}, nil)
		hc := func(test string) obj {
			return obj{"Healthcheck": obj{"Test": []string{"CMD-SHELL", test},
				"Interval": int64(200 * time.Millisecond), "Timeout": int64(time.Second), "Retries": 2}}
		}
		good := e.create("shop-db-1", service("db", "postgres:16", "echo up; exec sleep infinity", hc("true")))
		bad := e.create("shop-cache-1", service("cache", "redis:7", "echo up; exec sleep infinity", hc("false")))
		e.do("POST", "/containers/"+good+"/start", nil, nil)
		e.do("POST", "/containers/"+bad+"/start", nil, nil)
		time.Sleep(100 * time.Millisecond)
		h := e.inspect(good).State.Health
		if h == nil {
			t.Fatal("a started container with a HEALTHCHECK has no State.Health; compose's service_healthy would wait on it forever")
		}
		// A class whose exec doesn't enter the container yet (vm, before
		// plan item 9) is unhealthy at once, saying why, and never healthy.
		if h.Status == "unhealthy" && len(h.Log) > 0 && strings.Contains(h.Log[0].Output, "plan item 9") {
			t.Skip("expected: " + h.Log[0].Output)
		}
		for id, want := range map[string]string{good: "healthy", bad: "unhealthy"} {
			var got string
			if !eventually(10*time.Second, func() bool {
				h := e.inspect(id).State.Health
				got = h.Status
				return got == want
			}) {
				t.Errorf("%s: health %q, want %q", id, got, want)
			}
		}
	})
}

// What compose files can say that the facade refuses (§5's table):
// build: (501, use a pre-built image:), a host path volume (403, use a
// named volume), network_mode: host, privileged, cap_add and devices
// (403). Each is refused before anything is made.
func TestComposeRefused(t *testing.T) {
	eachClass(t, func(t *testing.T, e env) {
		if r := e.DockerDo(e.silo, "POST", "/v1.47/build?t=shop-web", nil); r.Code != 501 {
			t.Errorf("build: %d %s, want 501", r.Code, r.Body)
		}
		e.pull("nginx:1")
		for name, hc := range map[string]obj{
			"./data:/data":      {"Binds": []string{"/home/user/shop/data:/data:rw"}},
			"bind mount":        {"Mounts": []obj{{"Type": "bind", "Source": "/home/user/shop/data", "Target": "/data"}}},
			"network_mode host": {"NetworkMode": "host"},
			"privileged":        {"Privileged": true},
			"cap_add":           {"CapAdd": []string{"SYS_ADMIN"}},
			"devices":           {"Devices": []obj{{"PathOnHost": "/dev/fuse", "PathInContainer": "/dev/fuse", "CgroupPermissions": "rwm"}}},
		} {
			hc["Runtime"] = e.class
			r := e.DockerDo(e.silo, "POST", "/v1.47/containers/create?name=shop-web-1",
				obj{"Image": "nginx:1", "Labels": project("shop", "web"), "HostConfig": hc})
			if r.Code != 403 {
				t.Errorf("%s: %d %s, want 403", name, r.Code, r.Body)
			}
		}
		if left := e.listed("com.docker.compose.project=shop"); len(left) != 0 {
			t.Errorf("refused creates left %v", left)
		}
	})
}
