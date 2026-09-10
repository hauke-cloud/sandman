package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, args ...string) (*Config, error) {
	t.Helper()
	return Load(args, io.Discard)
}

func mustLoad(t *testing.T, args ...string) *Config {
	t.Helper()
	cfg, err := load(t, args...)
	if err != nil {
		t.Fatalf("Load(%v): %v", args, err)
	}
	return cfg
}

func TestDefaults(t *testing.T) {
	cfg := mustLoad(t)

	if cfg.ListenAddress != DefaultListenAddress {
		t.Fatalf("listen address = %q, want %q", cfg.ListenAddress, DefaultListenAddress)
	}
	if cfg.MetricsPath != DefaultMetricsPath {
		t.Fatalf("metrics path = %q", cfg.MetricsPath)
	}
	if !cfg.InstallCRDs {
		t.Fatal("the CRD is installed by the controller by default; that is the point of embedding it")
	}
	if !cfg.LeaderElection {
		t.Fatal("leader election defaults on, so a rollout never has two replicas acting")
	}
	if cfg.ReadOnly {
		t.Fatal("read-only defaults off")
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("log level = %v, want info", cfg.LogLevel)
	}
}

func TestFlagsWinOverTheEnvironment(t *testing.T) {
	t.Setenv("LISTEN_ADDRESS", ":9999")

	if got := mustLoad(t).ListenAddress; got != ":9999" {
		t.Fatalf("listen address = %q, want the environment's", got)
	}
	if got := mustLoad(t, "-listen-address", ":7777").ListenAddress; got != ":7777" {
		t.Fatalf("listen address = %q, want the flag's", got)
	}
}

func TestBooleansComeFromTheEnvironment(t *testing.T) {
	t.Setenv("INSTALL_CRDS", "false")
	t.Setenv("LEADER_ELECTION", "false")
	t.Setenv("API_READ_ONLY", "true")

	cfg := mustLoad(t)

	if cfg.InstallCRDs || cfg.LeaderElection || !cfg.ReadOnly {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestUnparseableBooleanIsRejected(t *testing.T) {
	t.Setenv("INSTALL_CRDS", "maybe")

	if _, err := load(t); err == nil {
		t.Fatal("a boolean that is neither true nor false must be rejected rather than guessed")
	}
}

func TestTokenIsReadFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Setenv("API_TOKEN_FILE", path)

	if got := mustLoad(t).APIToken; got != "s3cret" {
		t.Fatalf("token = %q, want the file's contents trimmed", got)
	}
}

func TestMissingTokenFileIsAnError(t *testing.T) {
	t.Setenv("API_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))

	if _, err := load(t); err == nil {
		t.Fatal("a token file that cannot be read must fail loudly, not start an unauthenticated API")
	}
}

func TestTokenFileWinsOverTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Setenv("API_TOKEN", "from-env")
	t.Setenv("API_TOKEN_FILE", path)

	if got := mustLoad(t).APIToken; got != "from-file" {
		t.Fatalf("token = %q, want the file's: it is the form that keeps the token out of the process environment", got)
	}
}

func TestNamespaceComesFromThePod(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "infra")

	if got := mustLoad(t).Namespace; got != "infra" {
		t.Fatalf("namespace = %q", got)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "empty listen address", args: []string{"-listen-address", ""}, want: "listen-address"},
		{name: "relative metrics path", args: []string{"-metrics-path", "metrics"}, want: "metrics-path"},
		{name: "metrics path under the api", args: []string{"-metrics-path", "/api/v1/metrics"}, want: "/api/"},
		{name: "empty namespace", args: []string{"-namespace", ""}, want: "namespace"},
		{name: "empty lease name", args: []string{"-leader-election-id", ""}, want: "leader-election-id"},
		{name: "unknown log format", args: []string{"-log-format", "xml"}, want: "log-format"},
		{name: "unknown log level", args: []string{"-log-level", "loud"}, want: "log level"},
		{name: "stray argument", args: []string{"unexpected"}, want: "unexpected"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.args...)
			if err == nil {
				t.Fatalf("Load(%v) succeeded, want an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLeaseNameMayBeEmptyWithoutLeaderElection(t *testing.T) {
	if _, err := load(t, "-leader-election=false", "-leader-election-id", ""); err != nil {
		t.Fatalf("an unused lease name should not be validated: %v", err)
	}
}

func TestLogLevels(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"WARNING": slog.LevelWarn,
		"error":   slog.LevelError,
	} {
		if got := mustLoad(t, "-log-level", name).LogLevel; got != want {
			t.Fatalf("log level %q = %v, want %v", name, got, want)
		}
	}
}
