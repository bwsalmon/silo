package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// CABRule defines an allowed resource and permission set in a Credential Access Boundary.
type CABRule struct {
	AvailableResource    string   `json:"availableResource"`
	AvailablePermissions []string `json:"availablePermissions"`
}

// CABPolicy defines the Credential Access Boundary policy payload.
type CABPolicy struct {
	AccessBoundaryRules []CABRule `json:"accessBoundaryRules"`
}

// ProjectCABPolicy creates a CAB policy bounding tokens to the specified project number.
func ProjectCABPolicy(projectNumber string) *CABPolicy {
	resource := fmt.Sprintf("//cloudresourcemanager.googleapis.com/projects/%s", projectNumber)
	return &CABPolicy{
		AccessBoundaryRules: []CABRule{
			{
				AvailableResource:    resource,
				AvailablePermissions: []string{"*"},
			},
		},
	}
}

// TokenProvider defines the interface for minting scoped GCP access tokens.
type TokenProvider interface {
	GetToken(ctx context.Context, serviceAccount string, policy *CABPolicy) (token string, expiry time.Time, err error)
}

// MockTokenProvider provides simulated tokens for offline testing and verification.
type MockTokenProvider struct {
	MintedCount int
	LastPolicy  *CABPolicy
	LastSA      string
}

// GetToken returns a simulated downscoped token.
func (m *MockTokenProvider) GetToken(ctx context.Context, serviceAccount string, policy *CABPolicy) (string, time.Time, error) {
	if serviceAccount == "" {
		return "", time.Time{}, errors.New("service account is required")
	}

	m.MintedCount++
	m.LastSA = serviceAccount
	m.LastPolicy = policy

	expiry := time.Now().Add(1 * time.Hour)
	token := fmt.Sprintf("mock-ya29-downscoped-%s-%d", serviceAccount, m.MintedCount)
	return token, expiry, nil
}

// STSTokenProvider uses the Google Security Token Service to exchange tokens with CAB.
type STSTokenProvider struct {
	HTTPClient  *http.Client
	STSEndpoint string
	BaseTokenFn func(ctx context.Context, sa string) (string, error)
}

// NewSTSTokenProvider creates a live Google Cloud STS token provider.
func NewSTSTokenProvider(baseTokenFn func(ctx context.Context, sa string) (string, error)) *STSTokenProvider {
	return &STSTokenProvider{
		HTTPClient:  &http.Client{Timeout: 10 * time.Second},
		STSEndpoint: "https://sts.googleapis.com/v1/token",
		BaseTokenFn: baseTokenFn,
	}
}

// GetToken fetches a parent token and downscopes it via Google STS.
func (s *STSTokenProvider) GetToken(ctx context.Context, serviceAccount string, policy *CABPolicy) (string, time.Time, error) {
	if s.BaseTokenFn == nil {
		return "", time.Time{}, errors.New("BaseTokenFn is required")
	}

	parentToken, err := s.BaseTokenFn(ctx, serviceAccount)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("fetch parent token: %w", err)
	}

	// If no policy is requested, return the parent token with 1h default expiry
	if policy == nil || len(policy.AccessBoundaryRules) == 0 {
		return parentToken, time.Now().Add(1 * time.Hour), nil
	}

	// Prepare CAB JSON options
	boundaryMap := map[string]interface{}{
		"accessBoundary": policy,
	}
	optionsJSON, err := json.Marshal(boundaryMap)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal access boundary: %w", err)
	}

	// Build RFC 8693 Token Exchange form values
	data := url.Values{}
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	data.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("subject_token", parentToken)
	data.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("options", string(optionsJSON))

	req, err := http.NewRequestWithContext(ctx, "POST", s.STSEndpoint, bytes.NewBufferString(data.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create STS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("call STS endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read STS response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("STS exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var stsResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &stsResp); err != nil {
		return "", time.Time{}, fmt.Errorf("parse STS response: %w", err)
	}

	expiry := time.Now().Add(time.Duration(stsResp.ExpiresIn) * time.Second)
	return stsResp.AccessToken, expiry, nil
}
