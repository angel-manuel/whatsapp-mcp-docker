// Package config loads and validates runtime configuration from environment
// variables. Every variable documented in REQUIREMENTS.md "Configuration"
// table is resolved here.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Transport is the MCP transport mode.
type Transport string

// Transport modes.
const (
	TransportHTTP  Transport = "http"
	TransportStdio Transport = "stdio"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	Transport Transport
	BindAddr  string
	// BindAddrExplicit reports whether BIND_ADDR was set in the environment
	// (vs. falling back to the default). The stdio transport uses this to
	// decide whether the admin listener should be clamped to 127.0.0.1.
	BindAddrExplicit bool
	Port             int
	AdminPort        int
	DataDir          string
	LogLevel         string
	LogFormat        string
	AuthToken        string
	MTLSCAFile       string
	MTLSCertFile     string
	MTLSKeyFile      string
	PairDeviceName   string
	FFmpegPath       string
	EnablePprof      bool
}

// Load reads the process environment into a Config and validates it.
func Load() (*Config, error) {
	bindAddr, bindAddrExplicit := lookupNonEmpty("BIND_ADDR")
	if !bindAddrExplicit {
		bindAddr = "0.0.0.0"
	}
	cfg := &Config{
		Transport:        Transport(strings.ToLower(getEnv("TRANSPORT", "http"))),
		BindAddr:         bindAddr,
		BindAddrExplicit: bindAddrExplicit,
		DataDir:          getEnv("DATA_DIR", "/data"),
		LogLevel:         strings.ToLower(getEnv("LOG_LEVEL", "info")),
		LogFormat:        strings.ToLower(getEnv("LOG_FORMAT", "json")),
		AuthToken:        os.Getenv("AUTH_TOKEN"),
		MTLSCAFile:       os.Getenv("MTLS_CA_FILE"),
		MTLSCertFile:     os.Getenv("MTLS_CERT_FILE"),
		MTLSKeyFile:      os.Getenv("MTLS_KEY_FILE"),
		PairDeviceName:   getEnv("WHATSAPP_DEVICE_NAME", "whatsapp-mcp"),
		FFmpegPath:       getEnv("FFMPEG_PATH", "/usr/bin/ffmpeg"),
	}

	var err error
	if cfg.Port, err = getEnvInt("PORT", 8081); err != nil {
		return nil, err
	}
	if cfg.AdminPort, err = getEnvInt("ADMIN_PORT", 8082); err != nil {
		return nil, err
	}
	if cfg.EnablePprof, err = getEnvBool("ENABLE_PPROF", false); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces invariants that cannot be expressed in the field types.
// In particular: HTTP transport requires either AUTH_TOKEN or the full
// MTLS_CA_FILE/MTLS_CERT_FILE/MTLS_KEY_FILE trio.
func (c *Config) Validate() error {
	switch c.Transport {
	case TransportHTTP, TransportStdio:
	default:
		return fmt.Errorf("TRANSPORT must be %q or %q, got %q",
			TransportHTTP, TransportStdio, c.Transport)
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel)
	}

	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("LOG_FORMAT must be json|text, got %q", c.LogFormat)
	}

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("PORT must be 1-65535, got %d", c.Port)
	}
	if c.AdminPort < 1 || c.AdminPort > 65535 {
		return fmt.Errorf("ADMIN_PORT must be 1-65535, got %d", c.AdminPort)
	}
	if c.Port == c.AdminPort {
		return fmt.Errorf("PORT and ADMIN_PORT must differ (both %d)", c.Port)
	}

	if c.DataDir == "" {
		return errors.New("DATA_DIR must not be empty")
	}

	if c.Transport == TransportHTTP {
		return c.validateHTTPAuth()
	}
	return nil
}

// MTLSEnabled reports whether all three MTLS_* vars are set.
func (c *Config) MTLSEnabled() bool {
	return c.MTLSCAFile != "" && c.MTLSCertFile != "" && c.MTLSKeyFile != ""
}

func (c *Config) validateHTTPAuth() error {
	anyMTLS := c.MTLSCAFile != "" || c.MTLSCertFile != "" || c.MTLSKeyFile != ""
	if anyMTLS && !c.MTLSEnabled() {
		return errors.New("MTLS_CA_FILE, MTLS_CERT_FILE and MTLS_KEY_FILE must all be set together")
	}
	if c.MTLSEnabled() {
		return nil
	}
	if c.AuthToken == "" {
		return errors.New("HTTP transport requires AUTH_TOKEN or the full MTLS_CA_FILE/MTLS_CERT_FILE/MTLS_KEY_FILE trio")
	}
	return nil
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// lookupNonEmpty returns the env var value and whether it was both set and
// non-empty. An empty value is treated the same as unset for "explicit-ness".
func lookupNonEmpty(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func getEnvInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func getEnvBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}
