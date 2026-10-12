// Package gwtls is the mTLS between gateway replicas and silod
// (gateway.md "Placement", SV-89): with --gateway-replicas, gateway-only
// VMs with no service account reach silod on a control VM over TCP,
// and each end proves itself with a certificate from a CA that only
// silod holds.
//
// The CA's key is in silod's store, which only silod can log in to. A
// gateway VM makes its own key at boot and never lets it leave the VM:
// it writes a certificate request to its serial console, which only
// processes on that VM can write and which silod's role already reads
// (compute.instances.getSerialPortOutput). The leader signs it for that
// instance and hands the certificate back through the instance's
// metadata (compute.instances.setMetadata, which only silod holds), and
// renews it before it runs out (Issuer).
//
// Gateway certificates carry only the client-auth usage and silod's
// only the server-auth one, so neither can stand in for the other, and
// silod believes a client only if its certificate names a gateway.
package gwtls

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/store"
)

// The names install and silod share.
const (
	// Port is silod's gateway port for gateway replicas, on each
	// control VM.
	Port = 8444
	// ServerName is the name silod's certificate carries and gateways
	// check.
	ServerName = "silod.silo.internal"
	// MetadataCert and MetadataCA are the gateway VM's metadata keys
	// the leader writes: its certificate and the CA's, PEM.
	MetadataCert = "silo-gateway-cert"
	MetadataCA   = "silo-gateway-ca"
	// CSRMarker starts the serial console line a gateway VM writes its
	// certificate request on, base64 of the PEM.
	CSRMarker = "silo-gateway-csr: "
	// LabelRole is the label install puts on its MIGs' VMs; the
	// leader issues to those whose value is RoleGateway.
	LabelRole   = "silo-role"
	RoleGateway = "gateway"
	RoleControl = "control"
	// GatewaySubnet is install's gateway subnet, where only install
	// puts VMs, and GatewayNamePrefix the gateway MIG's VM names'.
	GatewaySubnet     = "silo-gateway"
	GatewayNamePrefix = "silo-gateway-"
	// SettingCA is the store setting holding the CA, certificate and
	// key, PEM.
	SettingCA = "gateway-ca"
	// uriScheme is the scheme of a gateway certificate's URI SAN,
	// silo-gateway://<instance>.
	uriScheme = "silo-gateway"
)

// How long certificates last (call: a week, renewed with two days
// left, so a gateway VM rides out two days of silod being down).
const (
	CertLifetime = 7 * 24 * time.Hour
	RenewBefore  = 2 * 24 * time.Hour
	caLifetime   = 10 * 365 * 24 * time.Hour
	// skew is how far before now a certificate starts, for clocks
	// that disagree.
	skew = 5 * time.Minute
)

// CA is the gateway CA: its certificate, and its key.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	pem  []byte // the certificate's PEM
}

// NewKey is a new P-256 key.
func NewKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// NewCA makes a CA.
func NewCA(now time.Time) (*CA, error) {
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "silo gateway CA"},
		NotBefore:             now.Add(-skew),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, pem: pemBlock("CERTIFICATE", der)}, nil
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// CertPEM is the CA's certificate, PEM.
func (ca *CA) CertPEM() []byte { return ca.pem }

// Marshal is the CA as the store keeps it: certificate and key, PEM.
func (ca *CA) Marshal() (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(ca.Key)
	if err != nil {
		return "", err
	}
	return string(ca.pem) + string(pemBlock("PRIVATE KEY", der)), nil
}

// ParseCA reads what Marshal wrote.
func ParseCA(s string) (*CA, error) {
	ca := &CA{}
	rest := []byte(s)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		switch b.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, err
			}
			ca.Cert, ca.pem = c, pemBlock("CERTIFICATE", b.Bytes)
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
			if err != nil {
				return nil, err
			}
			signer, ok := k.(crypto.Signer)
			if !ok {
				return nil, errors.New("gwtls: the CA's key can't sign")
			}
			ca.Key = signer
		}
	}
	if ca.Cert == nil || ca.Key == nil {
		return nil, errors.New("gwtls: the stored CA lacks its certificate or key")
	}
	return ca, nil
}

// EnsureCA reads the CA from the store, making it first if there is
// none. Only the leader calls it, so two replicas never both make one.
func EnsureCA(ctx context.Context, st store.Store, now time.Time) (*CA, error) {
	var ca *CA
	err := st.Tx(ctx, func(tx store.Tx) error {
		v, ok, err := store.ReadSetting(ctx, tx, SettingCA)
		if err != nil {
			return err
		}
		if ok {
			ca, err = ParseCA(v)
			return err
		}
		if ca, err = NewCA(now); err != nil {
			return err
		}
		m, err := ca.Marshal()
		if err != nil {
			return err
		}
		return store.WriteSetting(ctx, tx, SettingCA, m)
	})
	return ca, err
}

// LoadCA reads the CA from the store; ok is false while the leader
// hasn't made one.
func LoadCA(ctx context.Context, st store.Store) (*CA, bool, error) {
	v, ok, err := store.GetSetting(ctx, st, SettingCA)
	if err != nil || !ok {
		return nil, false, err
	}
	ca, err := ParseCA(v)
	return ca, err == nil, err
}

// CSR is a certificate request for key, PEM.
func CSR(key crypto.Signer) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "silo gateway"}}, key)
	if err != nil {
		return nil, err
	}
	return pemBlock("CERTIFICATE REQUEST", der), nil
}

// CSRLine is the serial console line carrying csr.
func CSRLine(csr []byte) string {
	return CSRMarker + base64.StdEncoding.EncodeToString(csr)
}

// LastCSR is the certificate request on the last CSRMarker line of a
// serial console's output, nil if none.
func LastCSR(console string) []byte {
	var out []byte
	for _, line := range strings.Split(console, "\n") {
		i := strings.Index(line, CSRMarker)
		if i < 0 {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line[i+len(CSRMarker):]))
		if err == nil {
			out = b
		}
	}
	return out
}

func parseCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	b, _ := pem.Decode(csrPEM)
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("gwtls: not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("gwtls: the request's signature: %w", err)
	}
	return csr, nil
}

// GatewayURI is the URI SAN of instance's certificate.
func GatewayURI(instance string) *url.URL {
	return &url.URL{Scheme: uriScheme, Host: instance}
}

// SignGateway signs csrPEM for the gateway VM instance: client auth
// only, for CertLifetime. Only the request's key is taken from it;
// the name is instance's, which the leader read the request from.
func (ca *CA) SignGateway(csrPEM []byte, instance string, now time.Time) ([]byte, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: instance},
		URIs:         []*url.URL{GatewayURI(instance)},
		NotBefore:    now.Add(-skew),
		NotAfter:     now.Add(CertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	return pemBlock("CERTIFICATE", der), nil
}

// serverCert is silod's own certificate for key: server auth only.
func (ca *CA) serverCert(key crypto.Signer, now time.Time) (*tls.Certificate, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: ServerName},
		DNSNames:     []string{ServerName},
		NotBefore:    now.Add(-skew),
		NotAfter:     now.Add(CertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// ParseCert reads a PEM certificate.
func ParseCert(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("gwtls: not a PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// GatewayName is the instance a verified client certificate names, or
// an error if it names no gateway.
func GatewayName(cert *x509.Certificate) (string, error) {
	for _, u := range cert.URIs {
		if u.Scheme == uriScheme && u.Host != "" {
			return u.Host, nil
		}
	}
	return "", errors.New("gwtls: the client certificate names no gateway")
}

// Server is silod's side: a tls.Config that asks every client for a
// gateway certificate from the CA in the store, and serves a
// certificate of silod's own that it signs with that CA and renews.
type Server struct {
	Store store.Store
	// Now is time.Now if nil.
	Now func() time.Time

	mu   sync.Mutex
	ca   *CA
	key  crypto.Signer
	cert *tls.Certificate
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// load reads the CA, until it has it.
func (s *Server) load(ctx context.Context) (*CA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ca != nil {
		return s.ca, nil
	}
	ca, ok, err := LoadCA(ctx, s.Store)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("gwtls: the leader hasn't made the gateway CA yet")
	}
	s.ca = ca
	return ca, nil
}

func (s *Server) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	ca, err := s.load(hello.Context())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.cert != nil && now.Before(s.cert.Leaf.NotAfter.Add(-RenewBefore)) {
		return s.cert, nil
	}
	if s.key == nil {
		k, err := NewKey()
		if err != nil {
			return nil, err
		}
		s.key = k
	}
	c, err := ca.serverCert(s.key, now)
	if err != nil {
		return nil, err
	}
	s.cert = c
	return c, nil
}

// Config is the listener's tls.Config.
func (s *Server) Config() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: s.certificate,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			ca, err := s.load(hello.Context())
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca.Cert)
			return &tls.Config{
				MinVersion:     tls.VersionTLS13,
				GetCertificate: s.certificate,
				// The client's chain is verified for client auth, so a
				// silod certificate (server auth only) is no client.
				ClientAuth: tls.RequireAndVerifyClientCert,
				ClientCAs:  pool,
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 {
						return errors.New("gwtls: no client certificate")
					}
					_, err := GatewayName(cs.PeerCertificates[0])
					return err
				},
			}, nil
		},
	}
}

// Client is a gateway's side: its key, the certificate the leader
// issued for it and the CA's, read from files the VM's boot
// configuration writes from metadata, afresh at every handshake so that
// a renewed certificate is used as soon as it lands.
type Client struct {
	CertFile, KeyFile, CAFile string
}

// Config is the dialer's tls.Config: the CA file as it is now, pinned,
// and silod's name. Go checks the server's chain for server auth, which
// no gateway certificate has. A dialer calls it for each connection.
func (c *Client) Config() (*tls.Config, error) {
	caPEM, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("gwtls: no CA certificate in %s", c.CAFile)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: ServerName,
		RootCAs:    pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("gwtls: this gateway's certificate: %w", err)
			}
			return &cert, nil
		},
	}, nil
}

// VerifyServer checks that chain is silod's: signed by the CA in caPEM,
// for ServerName and for server auth, which no gateway certificate is.
func VerifyServer(caPEM []byte, chain []*x509.Certificate) error {
	if len(chain) == 0 {
		return errors.New("gwtls: silod sent no certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("gwtls: no CA certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, DNSName: ServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

// SamePublicKey reports whether a certificate and a request are for
// the same key.
func SamePublicKey(cert *x509.Certificate, csrPEM []byte) bool {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return false
	}
	a, err1 := x509.MarshalPKIXPublicKey(cert.PublicKey)
	b, err2 := x509.MarshalPKIXPublicKey(csr.PublicKey)
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}
