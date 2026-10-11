package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/toolchan"
)

// The agent tool endpoints on port 80 (gateway.md, "Agent tool
// channels", SV-84; agents.md, "The tool channel", SV-106).
//
// Both have handlers of their own, and neither reaches the facade or
// the fence API. The binary is served by the gateway itself, from
// Options.ToolsDir. An attach is forwarded to silod, but to
// ToolsSilodPath, which silod serves only to the gateway (the API port
// forwards only /compute/v1/ and /silo/v1/), with the caller's silo and
// source address; silod binds it to an agent, or refuses it, and holds
// the channel.

// ToolsSilodPath is where the gateway forwards an attach on silod's
// socket.
const ToolsSilodPath = "/silod/gateway/tools/attach"

// OriginAddrHeader carries an attach's source address to silod, beside
// Silo-Origin. Like Silo-Origin, the gateway removes any a caller sent.
const OriginAddrHeader = "Silo-Origin-Addr"

// ToolsArches are the architectures silo-tools is built for.
var ToolsArches = []string{"amd64", "arm64"}

// ToolsBinaryName is silo-tools' file name for arch in Options.ToolsDir.
func ToolsBinaryName(arch string) string { return "silo-tools-linux-" + arch }

// portOrigin is a port 80 caller's silo, by its source IP, which must
// be in one of the silo's subnets; or the refusal.
func (g *Gateway) portOrigin(r *http.Request) (*Tables, string, netip.Addr, error) {
	t := g.tables.Load()
	if t == nil {
		return nil, "", netip.Addr{}, errTools(http.StatusServiceUnavailable, "silo: the gateway has no tables from silod yet; retry")
	}
	ip, ok := sourceIP(r)
	silo, subnet, known := t.Resolve(ip)
	if !ok || !known || !subnet {
		return nil, "", netip.Addr{}, errTools(http.StatusForbidden, fmt.Sprintf("silo: %s is in no silo's subnet", ip))
	}
	return t, silo, ip, nil
}

type toolsError struct {
	status int
	msg    string
}

func (e *toolsError) Error() string { return e.msg }

func errTools(status int, msg string) error { return &toolsError{status, msg} }

func writeToolsError(w http.ResponseWriter, err error) {
	var e *toolsError
	if errors.As(err, &e) {
		http.Error(w, e.msg, e.status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// serveToolsBinary serves silo-tools for ?arch= to any caller in a
// silo's subnet. No control policy applies: a sealed silo's agents get
// their tools (call 53).
func (g *Gateway) serveToolsBinary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "silo: GET only", http.StatusMethodNotAllowed)
		return
	}
	if _, _, _, err := g.portOrigin(r); err != nil {
		writeToolsError(w, err)
		return
	}
	arch := r.URL.Query().Get("arch")
	known := false
	for _, a := range ToolsArches {
		known = known || a == arch
	}
	if !known {
		http.Error(w, fmt.Sprintf("silo: arch %q: silo-tools is built for %s", arch, strings.Join(ToolsArches, " and ")), http.StatusBadRequest)
		return
	}
	if g.opts.ToolsDir == "" {
		http.Error(w, "silo: this gateway serves no silo-tools binary", http.StatusNotFound)
		return
	}
	f, err := os.Open(filepath.Join(g.opts.ToolsDir, ToolsBinaryName(arch)))
	if err != nil {
		log.Printf("gateway: serving silo-tools for %s: %v", arch, err)
		http.Error(w, "silo: no silo-tools binary for "+arch, http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, ToolsBinaryName(arch), st.ModTime(), f)
}

// serveToolsAttach forwards an attach to silod, from a caller known by
// its source IP, with its silo and address. silod's answer -- 101 and
// the channel, a redirect to the host's gateway, or a 403 -- goes back
// as it is.
func (g *Gateway) serveToolsAttach(w http.ResponseWriter, r *http.Request) {
	t, silo, ip, err := g.portOrigin(r)
	if err != nil {
		writeToolsError(w, err)
		return
	}
	for k := range r.Header {
		if isSiloHeader(k) {
			r.Header.Del(k)
		}
	}
	r.Header.Set(OriginHeader, FormatOrigin(silo, t.Version))
	r.Header.Set(OriginAddrHeader, ip.String())
	g.toolsProxy.ServeHTTP(w, r)
}

// newToolsProxy forwards to ToolsSilodPath on silod's socket, whatever
// path the call came on.
func (g *Gateway) newToolsProxy(tr http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "silod", "silod"
			pr.Out.URL.Path, pr.Out.URL.RawPath, pr.Out.URL.RawQuery = ToolsSilodPath, "", ""
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("gateway: forwarding a tool channel attach: %v", err)
			http.Error(w, "silo: silod is unreachable; retry", http.StatusServiceUnavailable)
		},
	}
}

// ToolChannels is silod's end of agents' tool channels.
type ToolChannels struct {
	// Agent binds an attach to the agent whose tool VM is at ip in silo
	// (fenceapi's Server.ToolChannelAgent): a *fenceapi.Error
	// PERMISSION_DENIED is a 403.
	Agent func(ctx context.Context, silo string, ip netip.Addr) (string, error)
	// Host is the gateway address of the control VM that hosts agent's
	// session, "" if it is this one. Nil is always this one, as with a
	// single control VM.
	Host func(ctx context.Context, agent string) (string, error)
	// Hub holds the channels.
	Hub *toolchan.Hub
}

// attach is silod's half of an attach: bind it to an agent by origin,
// redirect it to the session's host, or take the channel.
func (s *silod) attach(w http.ResponseWriter, r *http.Request) {
	tc := s.opts.Tools
	if tc == nil || tc.Agent == nil || tc.Hub == nil {
		http.Error(w, "silo: this silod holds no tool channels", http.StatusServiceUnavailable)
		return
	}
	h := r.Header.Values(OriginHeader)
	if len(h) != 1 {
		http.Error(w, "silo: an attach needs exactly one "+OriginHeader, http.StatusForbidden)
		return
	}
	silo, version, err := ParseOrigin(h[0])
	if err != nil {
		http.Error(w, "silo: "+err.Error(), http.StatusForbidden)
		return
	}
	a := r.Header.Values(OriginAddrHeader)
	if len(a) != 1 {
		http.Error(w, "silo: an attach needs exactly one "+OriginAddrHeader, http.StatusForbidden)
		return
	}
	ip, err := netip.ParseAddr(a[0])
	if err != nil {
		http.Error(w, "silo: malformed "+OriginAddrHeader, http.StatusForbidden)
		return
	}
	ctx := r.Context()
	var newest uint64
	err = s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		newest, err = Stale(ctx, tx, silo, version)
		return err
	})
	if err != nil {
		http.Error(w, "silo: checking the origin: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if newest != 0 {
		w.Header().Set("Retry-After", "1")
		http.Error(w, fmt.Sprintf("silo: the gateway's origin table (version %d) is older than change %d, which affects the caller; retry", version, newest), http.StatusServiceUnavailable)
		return
	}
	agent, err := tc.Agent(ctx, silo, ip)
	if err != nil {
		var fe *fenceapi.Error
		if errors.As(err, &fe) && fe.Code == fenceapi.PermissionDenied {
			http.Error(w, fe.Message, http.StatusForbidden)
			return
		}
		http.Error(w, "silo: binding the attach to an agent: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if tc.Host != nil {
		host, err := tc.Host(ctx, agent)
		if err != nil {
			http.Error(w, "silo: finding the session's host: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if host != "" {
			http.Redirect(w, r, "http://"+host+ToolsAttachPath, http.StatusTemporaryRedirect)
			return
		}
	}
	tc.Hub.Accept(w, r, agent)
}
