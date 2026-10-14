package registry

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
)

// Login is docker login's check (local-registry.md §7, "docker login";
// the Docker facade's Options.Login): it tries username and password
// against server, an upstream's host as the facade normalises it
// (docker.io for Docker Hub), through the egress proxy as silo, and
// keeps nothing. server is a configured upstream by name or alias, or
// any other registry at https://<server>, as a facade pull's is.
//
// It asks the upstream's /v2/ for its challenge, as docker login does:
// on a Bearer challenge the credentials are good if the token service
// gives a token for them (with no scope beyond the challenge's); on a
// Basic one, if /v2/ answers them; and an upstream that wants none
// takes any. Each host is held to silo's egress policy first. A bad
// credential is a 401 *Error; a refused host is 403 and an upstream
// that can't be reached 503, as a pull's are.
func (s *Service) Login(ctx context.Context, silo, server, username, password string) error {
	creds := &Credentials{Username: username, Password: password}
	cr := &cacheReq{caller: fenceapi.Caller{Silo: silo, Via: fenceapi.ViaInternal}, method: http.MethodGet,
		name: imagerepos.Name{Upstream: server}, facade: true, creds: creds}
	if err := s.begin(ctx, cr); err != nil {
		return asError(err)
	}
	f, err := s.fetcherFor(cr, creds)
	if err != nil {
		return asError(err)
	}
	if err := f.login(ctx); err != nil {
		return asError(cr.loginFailed(err))
	}
	return nil
}

// login makes docker login's requests of the upstream with f's
// credentials.
func (f *fetcher) login(ctx context.Context) error {
	base, err := url.Parse(f.cr.up.Endpoint)
	if err != nil {
		return err
	}
	v2 := base.JoinPath("/v2/")
	resp, err := f.roundTrip(ctx, http.MethodGet, v2, nil, HostAPI)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode/100 == 2:
		resp.Body.Close()
		return nil
	case resp.StatusCode != http.StatusUnauthorized:
		return statusErr(resp, "/v2/")
	}
	resp.Body.Close()
	ch := resp.Header.Get("Www-Authenticate")
	if scheme, _ := parseChallenge(ch); !strings.EqualFold(scheme, "basic") {
		return f.authenticate(ctx, ch)
	}
	resp, err = f.roundTrip(ctx, http.MethodGet, v2, http.Header{"Authorization": {basicAuth(f.creds)}}, HostAPI)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return statusErr(resp, "/v2/")
	}
	resp.Body.Close()
	return nil
}

// loginFailed is Login's refusal: the upstream's refusal of the
// credentials (a token service that doesn't answer 200 for them is
// one), a host silo's egress doesn't allow, or an upstream that failed.
func (cr *cacheReq) loginFailed(err error) error {
	var ue *upstreamErr
	if errors.As(err, &ue) && !ue.unreachable && (ue.status == http.StatusUnauthorized || ue.status == http.StatusForbidden) {
		return rerr(http.StatusUnauthorized, codeUnauthorized, "silo: %s refused the credentials", cr.up.Name)
	}
	return cr.upstreamFailed(err)
}
