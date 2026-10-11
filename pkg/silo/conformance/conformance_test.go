package conformance

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPickAddressSpaceAvoidsUsedRanges(t *testing.T) {
	used := []netip.Prefix{
		netip.MustParsePrefix("10.128.0.0/9"),
		netip.MustParsePrefix("10.100.4.0/24"),
		netip.MustParsePrefix("10.101.0.0/16"),
		netip.MustParsePrefix("10.96.0.0/12"), // 10.96-10.111
	}
	got, err := PickAddressSpace(used)
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParsePrefix("10.112.0.0/16"); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	gw, ctl := Ranges(got)
	if gw.String() != "10.112.255.0/24" || ctl.String() != "10.112.254.0/24" {
		t.Errorf("ranges %v %v", gw, ctl)
	}
	if !got.Contains(gw.Addr()) || !got.Contains(ctl.Addr()) {
		t.Errorf("ranges outside the space")
	}
}

func TestPickAddressSpaceFull(t *testing.T) {
	if _, err := PickAddressSpace([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}); err == nil {
		t.Fatal("picked a space in a full 10/8")
	}
}

func TestProbeScriptAndReport(t *testing.T) {
	s := ProbeScript([]Check{
		{ID: "db-5432", Kind: "tcp", Host: "10.1.254.2", Port: 5432},
		{ID: "db-icmp", Kind: "icmp", Host: "10.1.254.2"},
		{ID: "db-dns", Kind: "dns", Host: "x.sql.goog."},
		{ID: "b-udp", Kind: "udp", Host: "10.1.2.3", Port: 53},
		{ID: "self", Kind: "http", URL: "http://silo.internal:8080/silo/v1/self", Headers: []string{"Silo-Origin: x"}},
		{ID: "ins", Kind: "http", Method: "POST", URL: "http://silo.internal:8080/x", Body: `{"name":"it's"}`},
		{ID: "out", Kind: "http", URL: "https://example.com", Proxy: "http://silo.internal:3128"},
		{ID: "raw", Kind: "sh", Script: "echo 'a' | grep -q a"},
	})
	for _, want := range []string{
		"r 'self' h -X 'GET' -H 'Silo-Origin: x' 'http://silo.internal:8080/silo/v1/self'",
		`r 'ins' h -X 'POST' -H 'Content-Type: application/json' --data-binary '{"name":"it'\''s"}' 'http://silo.internal:8080/x'`,
		"r 'out' h -X 'GET' -x 'http://silo.internal:3128' 'https://example.com'",
		`r 'raw' bash -c 'echo '\''a'\'' | grep -q a'`,
		"nc -z -w 5 '10.1.254.2' 5432", "ping -c 2 -W 3 '10.1.254.2'", "getent hosts 'x.sql.goog.'", "nc -u -w 3 10.1.2.3 53", "SILO-PROBE-DONE"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	serial := "Oct 11 google_metadata_script_runner[1]: startup-script: SILO-PROBE db-5432 fail \n" +
		"SILO-PROBE db-dns ok 10.1.254.2 x.sql.goog\r\n" +
		"junk\nSILO-PROBE-DONE\n"
	got, done := ParseProbeReport(serial)
	want := map[string]ProbeResult{"db-5432": {OK: false}, "db-dns": {OK: true, Detail: "10.1.254.2 x.sql.goog"}}
	if !done || !reflect.DeepEqual(got, want) {
		t.Errorf("got %v %v, want %v", got, done, want)
	}
	if got := HTTPStatus(ProbeResult{OK: true, Detail: "403 {\"error\":{}}"}); got != 403 {
		t.Errorf("HTTPStatus = %d", got)
	}
	if got := HTTPStatus(ProbeResult{Detail: "curl: (7) refused"}); got != 0 {
		t.Errorf("HTTPStatus of a failed check = %d", got)
	}
	if _, done := ParseProbeReport("SILO-PROBE a ok\n"); done {
		t.Error("done before the last line")
	}
}

func TestLeftovers(t *testing.T) {
	objs := []Object{
		{Kind: "instance", Name: "grain"},
		{Kind: "instance", Name: "silo-control", Labels: map[string]string{"silo-install": "x"}},
		{Kind: "disk", Name: "k7f3qa-builder", Labels: map[string]string{"silo": "k7f3qa"}},
		{Kind: "firewallPolicy", Name: "silo-fence", Description: "silo-install: the fence"},
		{Kind: "network", Name: "silo", Description: "made by hand"},
	}
	got := Leftovers(objs)
	var names []string
	for _, o := range got {
		names = append(names, o.String())
	}
	want := []string{"disk k7f3qa-builder", "firewallPolicy silo-fence", "instance silo-control"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("got %v, want %v", names, want)
	}
}

func TestDriftFields(t *testing.T) {
	doc := []byte(`{"schemas":{"Instance":{"properties":{
		"name":{"description":"Name"},
		"id":{"description":"[Output Only] The id"},
		"workloadIdentityConfig":{"description":"new"},
		"satisfiesPzi":{"description":"[Output Only] x"},
		"partnerMetadata":{"description":"new too"}}}}}`)
	got, err := DriftFields(doc, "Instance", FacadeInstanceFields)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"partnerMetadata", "workloadIdentityConfig"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := DriftFields(doc, "Nope", nil); err == nil {
		t.Error("no error for a missing schema")
	}
}

// TestExporterServersAndTCPHello runs the exporter probe's servers and
// the importer's TCPHello here, to check what the real-GCP export cases
// read off a probe: the HTTP server echoes the path and Silo-* headers,
// and the TCP server tells a plain connection from one with a PROXY v2
// header and names the source either way.
func TestExporterServersAndTCPHello(t *testing.T) {
	for _, tool := range []string{"bash", "python3", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s here", tool)
		}
	}
	if out, err := exec.Command("bash", "-c", ExporterServers(20).Script).CombinedOutput(); err != nil {
		t.Skipf("the servers didn't start here (a port in use?): %v %s", err, out)
	}

	req, _ := http.NewRequest("GET", "http://127.0.0.1:7000/hello", nil)
	req.Header.Set("Silo-Caller", "k7f3qa")
	req.Header.Set("Silo-Caller-Path", "/team-a/ci")
	req.Header.Set("Other", "dropped")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"path=/hello\n", "Silo-Caller: k7f3qa\n", "Silo-Caller-Path: /team-a/ci\n"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("HTTP answer %q doesn't have %q", body, want)
		}
	}
	if strings.Contains(string(body), "Other") {
		t.Errorf("HTTP answer %q names a header that isn't Silo-*", body)
	}

	out, err := exec.Command("bash", "-c", TCPHello("x", "127.0.0.1", 7003).Script).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "noproxy src=127.0.0.1" {
		t.Errorf("TCPHello without PROXY: %v %q", err, out)
	}

	conn, err := net.Dial("tcp", "127.0.0.1:7003")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// PROXY v2: signature, PROXY command, TCP over IPv4, 12 bytes of
	// addresses and ports.
	hdr := append([]byte("\r\n\r\n\x00\r\nQUIT\n"), 0x21, 0x11, 0, 12, 10, 1, 2, 3, 10, 9, 9, 9, 0x9c, 0x40, 0x1b, 0x5b)
	if _, err := conn.Write(hdr); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "pp2 src=10.1.2.3\n" {
		t.Errorf("PROXY v2 connection: %v %q, want pp2 src=10.1.2.3", err, line)
	}
}
