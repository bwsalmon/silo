package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProjectCABPolicy(t *testing.T) {
	policy := ProjectCABPolicy("123456789012")
	if len(policy.AccessBoundaryRules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(policy.AccessBoundaryRules))
	}

	rule := policy.AccessBoundaryRules[0]
	expectedResource := "//cloudresourcemanager.googleapis.com/projects/123456789012"
	if rule.AvailableResource != expectedResource {
		t.Errorf("expected %s, got %s", expectedResource, rule.AvailableResource)
	}
}

func TestMockTokenProvider(t *testing.T) {
	mock := &MockTokenProvider{}
	policy := ProjectCABPolicy("123456789012")

	token, expiry, err := mock.GetToken(context.Background(), "test-sa@proj.iam.gserviceaccount.com", policy)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if expiry.IsZero() {
		t.Fatal("expected non-zero expiry")
	}
	if mock.LastSA != "test-sa@proj.iam.gserviceaccount.com" {
		t.Errorf("unexpected last SA: %s", mock.LastSA)
	}
}

func TestSTSTokenProvider_ExchangeSuccess(t *testing.T) {
	// Mock STS Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}

		grantType := r.Form.Get("grant_type")
		if grantType != "urn:ietf:params:oauth:grant-type:token-exchange" {
			t.Errorf("unexpected grant_type: %s", grantType)
		}

		options := r.Form.Get("options")
		var parsed struct {
			AccessBoundary CABPolicy `json:"accessBoundary"`
		}
		if err := json.Unmarshal([]byte(options), &parsed); err != nil {
			t.Fatalf("failed to parse options: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"access_token": "downscoped-test-token-abc123",
			"expires_in": 3600,
			"token_type": "Bearer"
		}`))
	}))
	defer server.Close()

	provider := NewSTSTokenProvider(func(ctx context.Context, sa string) (string, error) {
		return "parent-access-token", nil
	})
	provider.STSEndpoint = server.URL
	provider.HTTPClient = server.Client()

	policy := ProjectCABPolicy("987654321098")
	token, expiry, err := provider.GetToken(context.Background(), "sa@example.com", policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if token != "downscoped-test-token-abc123" {
		t.Errorf("expected downscoped token, got %s", token)
	}
	if expiry.IsZero() {
		t.Error("expected valid expiry")
	}
}
