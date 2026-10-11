package gwtls_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/gwtls"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
	compute "google.golang.org/api/compute/v1"
)

var ctx = context.Background()

func openStore(t *testing.T) store.Store {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	return db
}

// fleet is gateway VMs in memory: each made its key at boot and wrote
// its request on its console.
type fleet struct {
	mu   sync.Mutex
	vms  map[string]*gwtls.Gateway
	keys map[string][]byte // PEM, as the VM keeps it on its own disk
	sets int
}

func newFleet() *fleet { return &fleet{vms: map[string]*gwtls.Gateway{}, keys: map[string][]byte{}} }

// boot is a VM booting: a new key, and its request on the console.
func (f *fleet) boot(t *testing.T, name string) {
	t.Helper()
	key, err := gwtls.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := gwtls.CSR(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	console := "Booting...\n" + gwtls.CSRLine(csr) + "\nsilo-gateway.service started\n"
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.vms[name]
	if g == nil {
		g = &gwtls.Gateway{Name: name, Zone: "us-central1-a"}
		f.vms[name] = g
	}
	g.CSR = gwtls.LastCSR(console)
	f.keys[name] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func (f *fleet) Gateways(context.Context) ([]gwtls.Gateway, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gwtls.Gateway
	for _, g := range f.vms {
		out = append(out, *g)
	}
	return out, nil
}

func (f *fleet) SetCert(_ context.Context, zone, name string, cert, ca []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[name].Cert = cert
	f.sets++
	return nil
}

func (f *fleet) cert(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := gwtls.ParseCert(f.vms[name].Cert)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return c
}

// TestIssuerIssuesAndRotates is the leader's duty, in process: a
// certificate for each gateway that asked, for its own key and name and
// for client auth only; nothing again until it is due; a renewal two
// days before it runs out; and a new certificate when the VM comes back
// with a new key.
func TestIssuerIssuesAndRotates(t *testing.T) {
	db := openStore(t)
	f := newFleet()
	f.boot(t, "silo-gateway-abcd")
	f.boot(t, "silo-gateway-efgh")
	// A VM still booting has written no request yet: nothing for it.
	f.vms["silo-gateway-new1"] = &gwtls.Gateway{Name: "silo-gateway-new1", Zone: "us-central1-b"}
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	iss := &gwtls.Issuer{Store: db, Fleet: f, Now: func() time.Time { return now }}

	n, err := iss.Run(ctx)
	if err != nil || n != 2 {
		t.Fatalf("first run issued %d (%v), want 2", n, err)
	}
	ca, ok, err := gwtls.LoadCA(ctx, db)
	if err != nil || !ok {
		t.Fatalf("the leader made no CA: %v", err)
	}
	c := f.cert(t, "silo-gateway-abcd")
	if err := c.CheckSignatureFrom(ca.Cert); err != nil {
		t.Errorf("not signed by the store's CA: %v", err)
	}
	if name, err := gwtls.GatewayName(c); err != nil || name != "silo-gateway-abcd" {
		t.Errorf("certificate names %q (%v)", name, err)
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("gateway certificate usages %v, want client auth only", c.ExtKeyUsage)
	}
	if !gwtls.SamePublicKey(c, f.vms["silo-gateway-abcd"].CSR) {
		t.Error("the certificate is not for the key the VM asked with")
	}

	// Nothing is due: nothing is written.
	now = now.Add(time.Hour)
	if n, err := iss.Run(ctx); err != nil || n != 0 {
		t.Fatalf("second run issued %d (%v), want 0", n, err)
	}

	// Within RenewBefore of running out: both renewed, same key.
	now = c.NotAfter.Add(-gwtls.RenewBefore + time.Minute)
	if n, err := iss.Run(ctx); err != nil || n != 2 {
		t.Fatalf("renewal issued %d (%v), want 2", n, err)
	}
	renewed := f.cert(t, "silo-gateway-abcd")
	if !renewed.NotAfter.After(c.NotAfter) || !gwtls.SamePublicKey(renewed, f.vms["silo-gateway-abcd"].CSR) {
		t.Errorf("renewed certificate runs to %v (was %v)", renewed.NotAfter, c.NotAfter)
	}

	// The VM is recreated (a rolling upgrade) with a new key.
	f.boot(t, "silo-gateway-efgh")
	if n, err := iss.Run(ctx); err != nil || n != 1 {
		t.Fatalf("after a new key issued %d (%v), want 1", n, err)
	}
	if !gwtls.SamePublicKey(f.cert(t, "silo-gateway-efgh"), f.vms["silo-gateway-efgh"].CSR) {
		t.Error("the new certificate is not for the new key")
	}

	// A certificate from some other CA is replaced.
	other, err := gwtls.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := other.SignGateway(f.vms["silo-gateway-abcd"].CSR, "silo-gateway-abcd", now)
	if err != nil {
		t.Fatal(err)
	}
	f.vms["silo-gateway-abcd"].Cert = forged
	if n, err := iss.Run(ctx); err != nil || n != 1 {
		t.Fatalf("over a foreign certificate issued %d (%v), want 1", n, err)
	}

	// The CA is kept: a second leader uses the same one.
	again, err := gwtls.EnsureCA(ctx, db, now)
	if err != nil || !again.Cert.Equal(ca.Cert) {
		t.Errorf("EnsureCA made a new CA (%v)", err)
	}
}

func TestLastCSR(t *testing.T) {
	key, _ := gwtls.NewKey()
	a, _ := gwtls.CSR(key)
	key2, _ := gwtls.NewKey()
	b, _ := gwtls.CSR(key2)
	console := "x\r\n[  1.0] " + gwtls.CSRLine(a) + "\r\nnoise\n" + gwtls.CSRLine(b) + "\n" + gwtls.CSRMarker + "!!notbase64\n"
	if got := gwtls.LastCSR(console); string(got) != string(b) {
		t.Errorf("LastCSR = %q, want the last good request", got)
	}
	if gwtls.LastCSR("nothing here") != nil {
		t.Error("a console with no request gave one")
	}
}

// pki is a CA in a store, with one gateway issued a certificate, its
// files on disk as the gateway VM keeps them.
type pki struct {
	db     store.Store
	ca     *gwtls.CA
	client *gwtls.Client
}

func newPKI(t *testing.T) *pki {
	db := openStore(t)
	f := newFleet()
	f.boot(t, "silo-gateway-abcd")
	if _, err := (&gwtls.Issuer{Store: db, Fleet: f}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	ca, _, err := gwtls.LoadCA(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := &gwtls.Client{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem"), CAFile: filepath.Join(dir, "ca.pem")}
	for file, b := range map[string][]byte{c.CertFile: f.vms["silo-gateway-abcd"].Cert, c.KeyFile: f.keys["silo-gateway-abcd"], c.CAFile: ca.CertPEM()} {
		if err := os.WriteFile(file, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &pki{db: db, ca: ca, client: c}
}

// serve is silod's gateway listener on the server config.
func serve(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	l, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, err := gwtls.GatewayName(r.TLS.PeerCertificates[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		io.WriteString(w, "hello "+name)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

func clientConfig(t *testing.T, c *gwtls.Client) *tls.Config {
	t.Helper()
	cfg, err := c.Config()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func get(addr string, cfg *tls.Config) (string, error) {
	tr := &http.Transport{DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := &tls.Dialer{Config: cfg}
		return d.DialContext(ctx, network, addr)
	}}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://silod/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func TestMutualTLS(t *testing.T) {
	p := newPKI(t)
	addr := serve(t, (&gwtls.Server{Store: p.db}).Config())

	got, err := get(addr, clientConfig(t, p.client))
	if err != nil || got != "hello silo-gateway-abcd" {
		t.Fatalf("a gateway with its certificate: %q, %v", got, err)
	}

	// No client certificate: refused.
	noCert := clientConfig(t, p.client)
	noCert.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	if _, err := get(addr, noCert); err == nil {
		t.Error("a client with no certificate was served")
	}

	// A certificate from another CA, for the same name: refused.
	other, _ := gwtls.NewCA(time.Now())
	key, _ := gwtls.NewKey()
	csr, _ := gwtls.CSR(key)
	certPEM, _ := other.SignGateway(csr, "silo-gateway-abcd", time.Now())
	forged := clientConfig(t, p.client)
	forged.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		b, _ := pem.Decode(certPEM)
		return &tls.Certificate{Certificate: [][]byte{b.Bytes}, PrivateKey: key}, nil
	}
	if _, err := get(addr, forged); err == nil {
		t.Error("a certificate from another CA was believed")
	}
}

// TestGatewayCannotPoseAsSilod: a gateway's certificate, from the same
// CA, can't serve as silod's: it isn't for server auth or for
// ServerName, so another gateway refuses it.
func TestGatewayCannotPoseAsSilod(t *testing.T) {
	p := newPKI(t)
	pair, err := tls.LoadX509KeyPair(p.client.CertFile, p.client.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequestClientCert})
	_, err = get(addr, clientConfig(t, p.client))
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("a gateway posing as silod was believed (%v)", err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	caPEM, _ := os.ReadFile(p.client.CAFile)
	if err := gwtls.VerifyServer(caPEM, []*x509.Certificate{leaf}); err == nil {
		t.Error("VerifyServer took a gateway certificate")
	}
}

// TestServerWaitsForCA: before the leader made the CA, a replica's
// listener refuses handshakes rather than serving with none.
func TestServerWaitsForCA(t *testing.T) {
	p := newPKI(t)
	empty := openStore(t)
	addr := serve(t, (&gwtls.Server{Store: empty}).Config())
	if _, err := get(addr, clientConfig(t, p.client)); err == nil {
		t.Error("served with no CA")
	}
}

// TestOnlyGatewayVMsAreIssued: a tenant's VM in the host project, made
// through the facade with the gateway label and a gateway-like name, is
// not in the gateway subnet, so the leader never signs for it.
func TestOnlyGatewayVMsAreIssued(t *testing.T) {
	gw := func(mut func(*compute.Instance)) *compute.Instance {
		in := &compute.Instance{Name: "silo-gateway-abcd", Labels: map[string]string{gwtls.LabelRole: gwtls.RoleGateway},
			NetworkInterfaces: []*compute.NetworkInterface{{Subnetwork: "https://www.googleapis.com/compute/v1/projects/p/regions/us-central1/subnetworks/silo-gateway"}}}
		if mut != nil {
			mut(in)
		}
		return in
	}
	if !gwtls.IsGatewayVM(gw(nil)) {
		t.Error("the MIG's VM is refused")
	}
	for name, in := range map[string]*compute.Instance{
		"tenant subnet": gw(func(in *compute.Instance) {
			in.NetworkInterfaces[0].Subnetwork = "projects/p/regions/us-central1/subnetworks/i73lj2-default"
		}),
		"second interface": gw(func(in *compute.Instance) {
			in.NetworkInterfaces = append(in.NetworkInterfaces, &compute.NetworkInterface{Subnetwork: "x"})
		}),
		"no label":        gw(func(in *compute.Instance) { in.Labels = nil }),
		"tenant name":     gw(func(in *compute.Instance) { in.Name = "i73lj2-silo-gateway-abcd" }),
		"service account": gw(func(in *compute.Instance) { in.ServiceAccounts = []*compute.ServiceAccount{{Email: "x@y"}} }),
	} {
		if gwtls.IsGatewayVM(in) {
			t.Errorf("%s: taken for a gateway VM", name)
		}
	}
}
