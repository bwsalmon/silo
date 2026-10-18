package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/hub/server"
)

// TestServeRefusesDevSignOnOnKubernetes checks that `silo hub serve`
// will not sign humans in from the development user header in a
// Kubernetes pod, where the hub runs in production, whether the header
// is named by flag or by environment variable; and that it refuses
// before it listens.
func TestServeRefusesDevSignOnOnKubernetes(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	for name, args := range map[string][]string{
		"flag": {"-addr", addr, "-database", "sqlite::memory:", "-dev-user-header", "X-User"},
		"env":  {"-addr", addr, "-database", "sqlite::memory:"},
	} {
		if name == "env" {
			t.Setenv("SILO_HUB_DEV_USER_HEADER", "X-User")
		}
		err := serve(args)
		if err == nil || !strings.Contains(err.Error(), "refused on Kubernetes") {
			t.Errorf("%s: serve = %v, want the dev sign-on refused", name, err)
		}
		// Nothing listened.
		l, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			t.Errorf("%s: %s taken after the refusal: %v", name, addr, lerr)
		} else {
			l.Close()
		}
	}
}

// TestServeRefusesSQLiteOutsideDev checks that `silo hub serve` will
// not run on SQLite unless told it is local development (-dev,
// SILO_HUB_DEV or the development user header), and never in a
// Kubernetes pod, where the production store is PostgreSQL on Cloud
// SQL; and that it refuses before it listens.
func TestServeRefusesSQLiteOutsideDev(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	for _, c := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no dev flag", []string{"-database", "sqlite::memory:"}, nil, "local development and tests only"},
		{"bare path", []string{"-database", "/tmp/hub.db"}, nil, "local development and tests only"},
		{"dev env false", []string{"-database", "sqlite::memory:"}, map[string]string{"SILO_HUB_DEV": "false"}, "local development and tests only"},
		{"dev flag on Kubernetes", []string{"-database", "sqlite::memory:", "-dev"}, map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"}, "refused on Kubernetes"},
		{"dev env on Kubernetes", []string{"-database", "sqlite::memory:"}, map[string]string{"SILO_HUB_DEV": "1", "KUBERNETES_SERVICE_HOST": "10.0.0.1"}, "refused on Kubernetes"},
		{"env database on Kubernetes", nil, map[string]string{"SILO_HUB_DATABASE": "sqlite:/data/hub.db", "SILO_HUB_DEV": "true", "KUBERNETES_SERVICE_HOST": "10.0.0.1"}, "refused on Kubernetes"},
		{"no database", nil, nil, "-database is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			err := serve(append([]string{"-addr", addr}, c.args...))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("serve = %v, want an error containing %q", err, c.want)
			}
			l, lerr := net.Listen("tcp", addr)
			if lerr != nil {
				t.Errorf("%s taken after the refusal: %v", addr, lerr)
			} else {
				l.Close()
			}
		})
	}
}

// TestCheckDatabaseAllowsDev checks the cases the hub does start in:
// PostgreSQL anywhere, and SQLite only in local development.
func TestCheckDatabaseAllowsDev(t *testing.T) {
	none := func(string) string { return "" }
	k8s := func(k string) string {
		if k == "KUBERNETES_SERVICE_HOST" {
			return "10.0.0.1"
		}
		return ""
	}
	for _, c := range []struct {
		db     string
		dev    bool
		getenv func(string) string
	}{
		{"postgres://silo@127.0.0.1:5432/silo", false, k8s},
		{"postgresql://silo@127.0.0.1:5432/silo", false, none},
		{"sqlite::memory:", true, none},
		{"sqlite:/tmp/hub.db", true, none},
	} {
		if err := server.CheckDatabase(c.db, c.dev, c.getenv); err != nil {
			t.Errorf("CheckDatabase(%q, %v) = %v, want nil", c.db, c.dev, err)
		}
	}
}

func TestBuildServeConfigGrainfilesFlagsAndEnv(t *testing.T) {
	t.Run("from env vars", func(t *testing.T) {
		t.Setenv("SILO_HUB_DATABASE", "sqlite::memory:")
		t.Setenv("SILO_HUB_DEV", "true")
		t.Setenv("SILO_HUB_WEBHOOK_SECRET", "env-webhook-secret")
		t.Setenv("SILO_HUB_GITHUB_TOKEN", "ghp_env_token")
		t.Setenv("SILO_HUB_GITHUB_API_URL", "https://ghe.example.com/api/v3")
		t.Setenv("SILO_HUB_GITHUB_APP_ID", "4242")
		t.Setenv("SILO_HUB_GITHUB_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----")

		_, cfg, err := buildServeConfig(nil)
		if err != nil {
			t.Fatalf("buildServeConfig: %v", err)
		}
		if cfg.WebhookSecret != "env-webhook-secret" {
			t.Errorf("WebhookSecret = %q, want env-webhook-secret", cfg.WebhookSecret)
		}
		if cfg.GitHubToken != "ghp_env_token" {
			t.Errorf("GitHubToken = %q, want ghp_env_token", cfg.GitHubToken)
		}
		if cfg.GitHubAPIURL != "https://ghe.example.com/api/v3" {
			t.Errorf("GitHubAPIURL = %q, want https://ghe.example.com/api/v3", cfg.GitHubAPIURL)
		}
		if cfg.GitHubAppID != 4242 {
			t.Errorf("GitHubAppID = %d, want 4242", cfg.GitHubAppID)
		}
		if !strings.Contains(string(cfg.GitHubAppPrivateKey), "BEGIN RSA PRIVATE KEY") {
			t.Errorf("GitHubAppPrivateKey = %q", string(cfg.GitHubAppPrivateKey))
		}
	})

	t.Run("from flags and secret files", func(t *testing.T) {
		dir := t.TempDir()
		whFile := filepath.Join(dir, "webhook.secret")
		tokFile := filepath.Join(dir, "github.token")
		appKeyFile := filepath.Join(dir, "app.pem")
		if err := os.WriteFile(whFile, []byte("file-wh-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tokFile, []byte("ghp_file_token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(appKeyFile, []byte("file-app-pem"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, cfg, err := buildServeConfig([]string{
			"-database", "sqlite::memory:",
			"-dev",
			"-webhook-secret-file", whFile,
			"-github-token-file", tokFile,
			"-github-api-url", "https://api.github.test",
			"-github-app-id", "99",
			"-github-app-key-file", appKeyFile,
		})
		if err != nil {
			t.Fatalf("buildServeConfig: %v", err)
		}
		if cfg.WebhookSecret != "file-wh-secret" {
			t.Errorf("WebhookSecret = %q, want file-wh-secret", cfg.WebhookSecret)
		}
		if cfg.GitHubToken != "ghp_file_token" {
			t.Errorf("GitHubToken = %q, want ghp_file_token", cfg.GitHubToken)
		}
		if cfg.GitHubAPIURL != "https://api.github.test" {
			t.Errorf("GitHubAPIURL = %q, want https://api.github.test", cfg.GitHubAPIURL)
		}
		if cfg.GitHubAppID != 99 {
			t.Errorf("GitHubAppID = %d, want 99", cfg.GitHubAppID)
		}
		if string(cfg.GitHubAppPrivateKey) != "file-app-pem" {
			t.Errorf("GitHubAppPrivateKey = %q, want file-app-pem", string(cfg.GitHubAppPrivateKey))
		}
	})

	t.Run("node UI IAP audience flag and env", func(t *testing.T) {
		t.Setenv("SILO_HUB_NODE_UI_IAP_AUDIENCE", "/projects/1/global/backendServices/2")
		_, cfg, err := buildServeConfig([]string{"-database", "sqlite::memory:", "-dev"})
		if err != nil {
			t.Fatalf("buildServeConfig: %v", err)
		}
		if cfg.NodeUIIAPAudience != "/projects/1/global/backendServices/2" {
			t.Errorf("NodeUIIAPAudience = %q", cfg.NodeUIIAPAudience)
		}
		_, cfg, err = buildServeConfig([]string{"-database", "sqlite::memory:", "-dev", "-node-ui-iap-audience", "/projects/1/global/backendServices/3"})
		if err != nil {
			t.Fatalf("buildServeConfig: %v", err)
		}
		if cfg.NodeUIIAPAudience != "/projects/1/global/backendServices/3" {
			t.Errorf("NodeUIIAPAudience = %q", cfg.NodeUIIAPAudience)
		}
	})
}

func TestServeRefusesMissingWebhookSecretOnKubernetes(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	err = serve([]string{"-addr", addr, "-database", "postgres://silo@127.0.0.1:5432/silo", "-iap-audience", "/projects/1/global/backendServices/2"})
	if err == nil || !strings.Contains(err.Error(), "webhook secret") {
		t.Fatalf("serve without webhook secret on Kubernetes = %v, want webhook secret error", err)
	}
	// Nothing listened.
	l2, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		t.Fatalf("%s taken after refusal: %v", addr, lerr)
	}
	l2.Close()

	// With a webhook secret file, CheckWebhookSecret succeeds.
	dir := t.TempDir()
	whFile := filepath.Join(dir, "webhook-secret")
	if err := os.WriteFile(whFile, []byte("prod-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, cfg, err := buildServeConfig([]string{
		"-database", "postgres://silo@127.0.0.1:5432/silo",
		"-iap-audience", "/projects/1/global/backendServices/2",
		"-webhook-secret-file", whFile,
	})
	if err != nil {
		t.Fatalf("buildServeConfig with webhook-secret-file on Kubernetes: %v", err)
	}
	if cfg.WebhookSecret != "prod-secret" {
		t.Errorf("WebhookSecret = %q, want prod-secret", cfg.WebhookSecret)
	}
}

func TestCLISubcommands(t *testing.T) {
	dir := t.TempDir()
	db := "sqlite:" + filepath.Join(dir, "hub.db")

	if err := run([]string{"hub", "migrate", "-database", db}); err != nil {
		t.Fatalf("hub migrate: %v", err)
	}
	if err := run([]string{
		"hub", "team",
		"-database", db,
		"-migrate=false",
		"-id", "infra",
		"-name", "Infra",
		"-admin", "user:alice@example.com",
		"-member", "user:bob@example.com",
		"-repo", "bwsalmon/silo",
		"-lost-requeue-after", "10m",
	}); err != nil {
		t.Fatalf("hub team: %v", err)
	}
	if err := run([]string{
		"hub", "token",
		"-database", db,
		"-migrate=false",
		"-owner", "alice@example.com",
		"-node", "ws-1",
	}); err != nil {
		t.Fatalf("hub token: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	jsonOut := filepath.Join(dir, "conformance.json")
	if err := run([]string{
		"conformance",
		"-only", "register",
		"-json", jsonOut,
		"--", exe, "-test.run=^TestHelperConformanceGrain$", "--",
	}); err != nil {
		t.Fatalf("conformance: %v", err)
	}
	data, err := os.ReadFile(jsonOut)
	if err != nil || !strings.Contains(string(data), `"passed": true`) {
		t.Fatalf("conformance json = %q, err = %v", string(data), err)
	}
}

func TestHelperConformanceGrain(t *testing.T) {
	if os.Getenv("SILO_HUB_URL") == "" {
		return
	}
	if err := run([]string{"conformance", "grain"}); err != nil {
		t.Fatal(err)
	}
}
