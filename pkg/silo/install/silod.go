package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/client"
)

// frontDoorHost is the host every front-door request names; the
// transport sends it down the tunnel whatever it says.
const frontDoorHost = "http://silo-front-door"

// NewSilod is silod's API over transport, which carries each request
// through the front door (silo's IAP tunnel and ID token).
func NewSilod(transport http.RoundTripper) (Silod, error) {
	c, err := client.New(client.Options{Endpoint: frontDoorHost + "/silo/v1/", Transport: transport, UserAgent: "silo-install"})
	if err != nil {
		return nil, err
	}
	return &silodAPI{c: c, hc: &http.Client{Transport: transport, Timeout: 30 * time.Second}}, nil
}

type silodAPI struct {
	c  *client.Client
	hc *http.Client
}

// raw makes a call outside /silo/v1/ and decodes its JSON answer.
func (s *silodAPI) raw(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, frontDoorHost+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func (s *silodAPI) Health(ctx context.Context) (SilodHealth, error) {
	var h SilodHealth
	_, err := s.raw(ctx, http.MethodGet, "/silod/health", nil, &h)
	return h, err
}

func (s *silodAPI) Bootstrap(ctx context.Context, req *BootstrapRequest) error {
	_, err := s.raw(ctx, http.MethodPost, "/silod/bootstrap", req, nil)
	return err
}

func (s *silodAPI) Root(ctx context.Context) (*RootSilo, error) {
	r, err := s.c.Describe(ctx, "/", false)
	if err != nil {
		return nil, err
	}
	out := &RootSilo{Path: r.Path, Regions: r.Regions, Blocks: r.Address.Blocks, Quota: map[string]map[string]string{}}
	for reg, classes := range r.Quota {
		out.Quota[reg] = map[string]string{}
		for c, q := range classes {
			out.Quota[reg][c] = q.Granted.String()
		}
	}
	return out, nil
}

func (s *silodAPI) Members(ctx context.Context, silo string) ([]string, error) {
	ms, err := s.c.Members(ctx, silo)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range ms {
		out = append(out, m.Identity)
	}
	return out, nil
}

func (s *silodAPI) PutMember(ctx context.Context, silo, identity string) error {
	_, err := s.c.PutMember(ctx, silo, identity, client.Mutation{})
	return err
}

func (s *silodAPI) Regions(ctx context.Context) ([]string, error) {
	self, err := s.c.Describe(ctx, "self", false)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if _, err := s.raw(ctx, http.MethodGet, "/compute/v1/projects/"+url.PathEscape(self.Facade.Project)+"/regions", nil, &list); err != nil {
		return nil, err
	}
	var out []string
	for _, it := range list.Items {
		out = append(out, it.Name)
	}
	return out, nil
}
