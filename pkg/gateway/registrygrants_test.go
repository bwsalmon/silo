package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
)

// regEnv is env with silod's registry service over Memory, its blob
// GETs slowed down so that a pull is in flight for seconds.
type regEnv struct {
	*env
	reg atomic.Pointer[registry.Service]
	// b is a sibling of a's, full, with a subnet.
	b string
}

func newRegEnv(t *testing.T) *regEnv {
	t.Helper()
	r := &regEnv{}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/blobs/sha256:") {
			w = &slowWriter{ResponseWriter: w}
		}
		r.reg.Load().ServeHTTP(w, req)
	})
	r.env = newEnv(t, envOptions{registry: h})
	r.b = r.mk("-"+r.root, "b", 16, obj{"regions": []string{"us-central1"}, "quota": obj{"global": obj{"subnetworks": 1}}})
	r.mkSubnet(r.b, false)
	r.sync()
	r.reg.Store(registry.New(registry.Options{Store: r.db, API: r.fence, Content: registry.NewMemory(), MaxChunk: 16 << 20}))
	r.tx(func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, r.root, []ledger.Amount{{Key: fenceapi.ImageReposClass, Amount: ledger.Units(10)},
			{Key: registry.DiskClass, Amount: ledger.Units(2)}}); err != nil {
			return err
		}
		for _, s := range []string{r.a, r.b} {
			if err := ledger.Grant(ctx, tx, s, []ledger.Amount{{Key: fenceapi.ImageReposClass, Amount: ledger.Units(4)},
				{Key: registry.DiskClass, Amount: ledger.Units(1)}}); err != nil {
				return err
			}
		}
		return nil
	})
	return r
}

// slowWriter writes a blob 32 KiB at a time, 20ms apart.
type slowWriter struct{ http.ResponseWriter }

func (w *slowWriter) Write(b []byte) (int, error) {
	n := 0
	for len(b) > 0 {
		k := min(len(b), 32<<10)
		m, err := w.ResponseWriter.Write(b[:k])
		n += m
		if err != nil {
			return n, err
		}
		w.Flush()
		b = b[k:]
		time.Sleep(20 * time.Millisecond)
	}
	return n, nil
}

func (w *slowWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

// regDo makes a registry request on port 80 from ip.
func (e *regEnv) regDo(ip netip.Addr, method, path, ctype string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://registry.silo.internal"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialFrom(ctx, e.web80, ip)
	}}}
	return c.Do(req)
}

func (e *regEnv) reg80(ip netip.Addr, method, path, ctype string, body []byte) (int, http.Header, string) {
	e.t.Helper()
	resp, err := e.regDo(ip, method, path, ctype, body)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// pushImage pushes a one-layer image to name:tag from ip and returns
// the layer's digest.
func (e *regEnv) pushImage(ip netip.Addr, name, tag string, layer []byte) string {
	e.t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	var ds []string
	for _, b := range [][]byte{cfg, layer} {
		d := registry.Digest("sha256", b)
		if code, _, body := e.reg80(ip, "POST", "/v2/"+name+"/blobs/uploads/?digest="+d, "application/octet-stream", b); code != 201 {
			e.t.Fatalf("pushing a blob to %s: %d %s", name, code, body)
		}
		ds = append(ds, d)
	}
	m, _ := json.Marshal(obj{"schemaVersion": 2, "mediaType": registry.MediaOCIManifest,
		"config": obj{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": ds[0], "size": len(cfg)},
		"layers": []obj{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": ds[1], "size": len(layer)}}})
	if code, _, body := e.reg80(ip, "PUT", "/v2/"+name+"/manifests/"+tag, registry.MediaOCIManifest, m); code != 201 {
		e.t.Fatalf("pushing %s:%s: %d %s", name, tag, code, body)
	}
	return ds[1]
}

// startPull starts a GET of path from ip and reads its first bytes. The
// channel it returns gets the error that ended the read: one starting
// "finished" if the body was read to its end.
func (e *regEnv) startPull(ip netip.Addr, path string) <-chan error {
	e.t.Helper()
	resp, err := e.regDo(ip, "GET", path, "", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		e.t.Fatalf("pull %s: %d", path, resp.StatusCode)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 1024)); err != nil {
		e.t.Fatalf("pull %s: %v", path, err)
	}
	done := make(chan error, 1)
	go func() {
		defer resp.Body.Close()
		n, err := io.Copy(io.Discard, resp.Body)
		if err == nil {
			err = fmt.Errorf("finished, %d more bytes", n)
		}
		done <- err
	}()
	return done
}

// cutOff waits for a pull or push in flight to be closed, within a
// second of start.
func cutOff(t *testing.T, what string, done <-chan error, start time.Time) {
	t.Helper()
	select {
	case err := <-done:
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s was cut off after %v", what, d)
		}
		if err != nil && strings.HasPrefix(err.Error(), "finished") {
			t.Errorf("%s was not cut off: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s is still open", what)
	}
}

// When a registry export is withdrawn, the importer's pull of the
// exporter's repo in flight is closed within a second, and it pulls
// nothing more, while the exporter's own pull goes on
// (local-registry.md §12, "exports").
func TestRegistryExportWithdrawn(t *testing.T) {
	e := newRegEnv(t)
	e.allowExports("a", "b")
	big := bytes.Repeat([]byte("layer!"), 700<<10) // 4.2 MB, about 2.7s slowed
	layer := e.pushImage(e.ip[e.a], "a/base", "1", big)
	e.ok(e.a, "POST", "/silo/v1/self/exports", obj{"name": "imgs", "protocol": "registry", "repos": []string{"base"}, "to": []string{"/b"}})
	if code, _, _ := e.reg80(e.ip[e.b], "GET", "/v2/a/base/manifests/1", "", nil); code != 404 {
		t.Fatalf("offered, not accepted: %d", code)
	}
	e.ok(e.b, "POST", "/silo/v1/self/imports", obj{"export": "/a/imgs"})
	e.sync()
	if code, _, body := e.reg80(e.ip[e.b], "GET", "/v2/a/base/manifests/1", "", nil); code != 200 {
		t.Fatalf("accepted: %d %s", code, body)
	}
	if code, _, _ := e.reg80(e.ip[e.b], "POST", "/v2/a/base/blobs/uploads/", "", nil); code != 404 {
		t.Fatalf("the importer pushed: %d", code)
	}

	pull := e.startPull(e.ip[e.b], "/v2/a/base/blobs/"+layer)
	own := e.startPull(e.ip[e.a], "/v2/a/base/blobs/"+layer)
	start := time.Now()
	e.ok(e.a, "DELETE", "/silo/v1/self/exports/imgs", nil)
	cutOff(t, "the importer's pull", pull, start)
	if err := <-own; !strings.HasPrefix(err.Error(), "finished") {
		t.Errorf("the exporter's own pull: %v", err)
	}
	if code, _, _ := e.reg80(e.ip[e.b], "GET", "/v2/a/base/manifests/1", "", nil); code != 404 {
		t.Fatalf("after withdrawal: %d", code)
	}
}

// When a manage grant with registry in its scope is revoked, the
// holder's upload in flight to the target is closed within a second and
// abandoned, and its session is gone to the holder (local-registry.md
// §12, "manage grants").
func TestRegistryManageGrantRevoked(t *testing.T) {
	e := newRegEnv(t)
	gr := e.ok(e.root, "POST", "/silo/v1/silos/a/manageGrants", obj{"name": "rollout", "holder": "/b",
		"scope": obj{"facade": obj{"resources": []string{"registry"}, "access": "write"}}, "lease": obj{"ttl": "3600s"}})
	e.ok(e.b, "POST", "/silo/v1/self/manageGrants/"+gr["id"].(string)+":accept", obj{})
	e.sync()
	e.pushImage(e.ip[e.b], "a/app", "1", []byte("the holder's"))
	code, h, body := e.reg80(e.ip[e.b], "POST", "/v2/a/app/blobs/uploads/", "", nil)
	if code != 202 {
		t.Fatalf("start: %d %s", code, body)
	}
	loc := h.Get("Location")

	// A chunk in flight: its body is never finished.
	conn, err := dialFrom(ctx, e.web80, e.ip[e.b])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "PATCH "+loc+" HTTP/1.1\r\nHost: registry.silo.internal\r\n"+
		"Content-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n"+
		"5\r\nhello\r\n")
	done := make(chan error, 1)
	go func() {
		b, err := io.ReadAll(conn)
		if err == nil && len(b) > 0 {
			err = fmt.Errorf("finished: %q", b)
		}
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	e.ok(e.root, "DELETE", "/silo/v1/silos/a/manageGrants/rollout", nil)
	cutOff(t, "the holder's upload", done, start)
	if code, _, _ := e.reg80(e.ip[e.b], "GET", loc, "", nil); code != 404 {
		t.Errorf("the abandoned upload, to the holder: %d", code)
	}
	if code, _, _ := e.reg80(e.ip[e.b], "GET", "/v2/a/app/manifests/1", "", nil); code != 404 {
		t.Errorf("after revocation: %d", code)
	}
}
