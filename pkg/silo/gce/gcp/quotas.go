package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	"google.golang.org/api/googleapi"
)

// ListComputeQuotaInfos implements gce.Client: Cloud Quotas'
// quotaInfos.list for compute.googleapis.com, every page. The generated
// Cloud Quotas client isn't in google.golang.org/api, so this is the
// REST call itself, through the same retrying, authenticated client.
func (c *Client) ListComputeQuotaInfos(ctx context.Context, project string) ([]*gce.QuotaInfo, error) {
	base := c.quotas + "v1/projects/" + url.PathEscape(project) +
		"/locations/global/services/" + gce.CloudQuotasService + "/quotaInfos"
	var out []*gce.QuotaInfo
	token := ""
	for {
		q := url.Values{}
		if c.pageSize > 0 {
			q.Set("pageSize", strconv.FormatInt(c.pageSize, 10))
		}
		if token != "" {
			q.Set("pageToken", token)
		}
		u := base
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			QuotaInfos    []*gce.QuotaInfo `json:"quotaInfos"`
			NextPageToken string           `json:"nextPageToken"`
		}
		err = googleapi.CheckResponse(resp)
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&page)
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, page.QuotaInfos...)
		if page.NextPageToken == "" || page.NextPageToken == token {
			return out, nil
		}
		token = page.NextPageToken
	}
}
