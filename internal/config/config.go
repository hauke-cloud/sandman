// Package config assembles sandman's configuration from environment variables
// and command line flags. Flags win over the environment, which wins over the
// defaults.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultListenAddress    = ":8080"
	DefaultMetricsPath      = "/metrics"
	DefaultNamespace        = "sandman"
	DefaultLeaderElectionID = "sandman.hauke.cloud"
	DefaultShutdownTimeout  = 30 * time.Second
)

// Config is sandman's resolved configuration.
type Config struct {
	// ListenAddress is the address the REST API and the metrics endpoint are
	// served on. They share a port on purpose: everything here is either
	// public to the cluster or protected by the same token.
	ListenAddress string
	// MetricsPath is where the Prometheus handler is mounted.
	MetricsPath string
	// APIToken, when set, is required as a bearer token on every API route.
	APIToken string
	// ReadOnly refuses every state-changing route.
	ReadOnly bool
	// Namespace is where sandman runs. Shutdown pods and Secrets referenced
	// without a namespace land here.
	Namespace string
	// InstallCRDs applies the embedded CustomResourceDefinition at start-up.
	InstallCRDs bool
	// LeaderElection makes exactly one replica act. Reads and requests are
	// served by every replica regardless.
	LeaderElection bool
	// LeaderElectionID is the name of the lease.
	LeaderElectionID string
	// WOLSourceAddress pins the local address magic packets are sent from. On
	// a host with more than one network this is the difference between the
	// packet reaching the machines and disappearing into a cluster network.
	WOLSourceAddress string
	// LogLevel is the minimum level logged.
	LogLevel slog.Level
	// LogFormat is "text" or "json".
	LogFormat string
}

// Load resolves the configuration from args and the environment. args is the
// argument list without the program name.
func Load(args []string, output io.Writer) (*Config, error) {
	cfg := &Config{
		ListenAddress:    envString("LISTEN_ADDRESS", DefaultListenAddress),
		MetricsPath:      envString("METRICS_PATH", DefaultMetricsPath),
		Namespace:        envString("POD_NAMESPACE", envString("NAMESPACE", DefaultNamespace)),
		LeaderElectionID: envString("LEADER_ELECTION_ID", DefaultLeaderElectionID),
		WOLSourceAddress: envString("WOL_SOURCE_ADDRESS", ""),
		LogFormat:        envString("LOG_FORMAT", "text"),
	}

	token, err := loadToken()
	if err != nil {
		return nil, err
	}
	cfg.APIToken = token

	readOnly, err := envBool("API_READ_ONLY", false)
	if err != nil {
		return nil, err
	}
	installCRDs, err := envBool("INSTALL_CRDS", true)
	if err != nil {
		return nil, err
	}
	leaderElection, err := envBool("LEADER_ELECTION", true)
	if err != nil {
		return nil, err
	}
	logLevel := envString("LOG_LEVEL", "info")

	fs := flag.NewFlagSet("sandman", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.ListenAddress, "listen-address", cfg.ListenAddress, "address to serve the API and metrics on (env LISTEN_ADDRESS)")
	fs.StringVar(&cfg.MetricsPath, "metrics-path", cfg.MetricsPath, "path to serve metrics on (env METRICS_PATH)")
	fs.StringVar(&cfg.APIToken, "api-token", cfg.APIToken, "bearer token required by the API, empty for an unauthenticated API (env API_TOKEN, API_TOKEN_FILE)")
	fs.BoolVar(&cfg.ReadOnly, "read-only", readOnly, "refuse every request that would change a machine's power state (env API_READ_ONLY)")
	fs.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "namespace sandman runs in (env POD_NAMESPACE)")
	fs.BoolVar(&cfg.InstallCRDs, "install-crds", installCRDs, "apply the embedded CustomResourceDefinition at start-up (env INSTALL_CRDS)")
	fs.BoolVar(&cfg.LeaderElection, "leader-election", leaderElection, "elect one replica to act on devices (env LEADER_ELECTION)")
	fs.StringVar(&cfg.LeaderElectionID, "leader-election-id", cfg.LeaderElectionID, "name of the leader election lease (env LEADER_ELECTION_ID)")
	fs.StringVar(&cfg.WOLSourceAddress, "wol-source-address", cfg.WOLSourceAddress, "local address to send magic packets from, empty to let the kernel choose (env WOL_SOURCE_ADDRESS)")
	fs.StringVar(&logLevel, "log-level", logLevel, "log level: debug, info, warn, error (env LOG_LEVEL)")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format: text or json (env LOG_FORMAT)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if cfg.LogLevel, err = parseLogLevel(logLevel); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.ListenAddress == "" {
		return errors.New("listen-address must not be empty")
	}
	if !strings.HasPrefix(c.MetricsPath, "/") {
		return fmt.Errorf("metrics-path must start with %q, got %q", "/", c.MetricsPath)
	}
	if strings.HasPrefix(c.MetricsPath, "/api/") {
		// The API routes would shadow it, and the failure -- a scrape that
		// returns JSON -- is a confusing way to find that out.
		return fmt.Errorf("metrics-path must not be under /api/, got %q", c.MetricsPath)
	}
	if c.Namespace == "" {
		return errors.New("namespace must not be empty: set POD_NAMESPACE from the pod's own metadata")
	}
	if c.LeaderElection && c.LeaderElectionID == "" {
		return errors.New("leader-election-id must not be empty when leader election is on")
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("log-format must be text or json, got %q", c.LogFormat)
	}
	return nil
}

// loadToken reads the API token from API_TOKEN, or from the file named by
// API_TOKEN_FILE. The file form is what a mounted Kubernetes Secret wants,
// because it keeps the token out of the process environment.
func loadToken() (string, error) {
	if path := os.Getenv("API_TOKEN_FILE"); path != "" {
		// The path comes from the operator's own configuration, which is the
		// whole point of the option.
		content, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
		if err != nil {
			return "", fmt.Errorf("read API_TOKEN_FILE: %w", err)
		}
		return strings.TrimSpace(string(content)), nil
	}
	return strings.TrimSpace(os.Getenv("API_TOKEN")), nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}
