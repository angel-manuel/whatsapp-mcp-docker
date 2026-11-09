package config

import (
	"strings"
	"testing"
	"time"
)

// allVars lists every env var the loader reads. Each test clears them before
// setting only what it wants, so tests do not leak values across cases.
var allVars = []string{
	"TRANSPORT", "BIND_ADDR", "PORT", "DATA_DIR",
	"LOG_LEVEL", "LOG_FORMAT", "AUTH_TOKEN", "ADMIN_TOKEN",
	"MAX_SESSIONS", "STDIO_SESSION", "DATABASE_URL",
	"MTLS_CA_FILE", "MTLS_CERT_FILE", "MTLS_KEY_FILE",
	"WHATSAPP_DEVICE_NAME", "FFMPEG_PATH", "ENABLE_PPROF",
	"MEDIA_MAX_BYTES", "MEDIA_TTL", "MEDIA_SWEEP_INTERVAL",
	"MEDIA_MAX_UPLOAD_BYTES",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allVars {
		t.Setenv(k, "")
	}
}

func TestLoad_DefaultsStdio(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Transport != TransportStdio {
		t.Errorf("Transport = %q, want %q", cfg.Transport, TransportStdio)
	}
	if cfg.BindAddr != "0.0.0.0" {
		t.Errorf("BindAddr = %q, want 0.0.0.0", cfg.BindAddr)
	}
	if cfg.Port != 8081 {
		t.Errorf("Port = %d, want 8081", cfg.Port)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir = %q, want /data", cfg.DataDir)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.PairDeviceName != "whatsapp-mcp" {
		t.Errorf("PairDeviceName = %q, want whatsapp-mcp", cfg.PairDeviceName)
	}
	if cfg.FFmpegPath != "/usr/bin/ffmpeg" {
		t.Errorf("FFmpegPath = %q, want /usr/bin/ffmpeg", cfg.FFmpegPath)
	}
	if cfg.EnablePprof {
		t.Errorf("EnablePprof = true, want false")
	}
}

func TestLoad_HTTPWithoutAdminToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "http")

	// ADMIN_TOKEN is optional: the admin API is then anonymous and relies
	// on the ingress. Session tokens still gate /mcp and /media.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminToken != "" {
		t.Errorf("AdminToken = %q, want empty", cfg.AdminToken)
	}
	if cfg.MaxSessions != DefaultMaxSessions {
		t.Errorf("MaxSessions = %d, want %d", cfg.MaxSessions, DefaultMaxSessions)
	}
	if cfg.StdioSession != "default" {
		t.Errorf("StdioSession = %q, want default", cfg.StdioSession)
	}
	if got := cfg.SessionsDir(); got != "/data/sessions" {
		t.Errorf("SessionsDir() = %q", got)
	}
	if got := cfg.RegistryPath(); got != "/data/sessions.db" {
		t.Errorf("RegistryPath() = %q", got)
	}
}

func TestLoad_AdminTokenAndMaxSessions(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "http")
	t.Setenv("ADMIN_TOKEN", "secret")
	t.Setenv("MAX_SESSIONS", "3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminToken != "secret" {
		t.Errorf("AdminToken = %q", cfg.AdminToken)
	}
	if cfg.MaxSessions != 3 {
		t.Errorf("MaxSessions = %d", cfg.MaxSessions)
	}
}

// TestLoad_LegacyAuthTokenFatal pins the breaking change: a 0.7 deployment
// that still sets AUTH_TOKEN must fail loudly and point at the migration,
// not boot with a token that no longer protects anything.
func TestLoad_LegacyAuthTokenFatal(t *testing.T) {
	for _, transport := range []string{"http", "stdio"} {
		t.Run(transport, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TRANSPORT", transport)
			t.Setenv("AUTH_TOKEN", "secret")

			_, err := Load()
			if err == nil {
				t.Fatal("Load: want error, got nil")
			}
			for _, want := range []string{"AUTH_TOKEN", "POST /sessions", "README"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should mention %q", err, want)
				}
			}
		})
	}
}

func TestLoad_SessionSettingsRejectBadValues(t *testing.T) {
	cases := []struct{ key, value string }{
		{"MAX_SESSIONS", "0"},
		{"MAX_SESSIONS", "-1"},
		{"MAX_SESSIONS", "lots"},
		{"STDIO_SESSION", "../etc"},
		{"STDIO_SESSION", "Default"},
		{"STDIO_SESSION", "a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TRANSPORT", "stdio")
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load: want error for %s=%q", tc.key, tc.value)
			}
		})
	}
}

func TestLoad_DatabaseURL(t *testing.T) {
	for _, url := range []string{
		"postgres://u:p@db:5432/wa?sslmode=disable",
		"postgresql://u@db/wa",
	} {
		clearEnv(t)
		t.Setenv("TRANSPORT", "stdio")
		t.Setenv("DATABASE_URL", url)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(%q): %v", url, err)
		}
		if cfg.DatabaseURL != url {
			t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, url)
		}
	}
}

func TestLoad_DatabaseURLRejectsOtherSchemesWithoutEchoingIt(t *testing.T) {
	for _, url := range []string{"mysql://u:hunter2@db/wa", "sqlite:///data/x.db", "hunter2"} {
		clearEnv(t)
		t.Setenv("TRANSPORT", "stdio")
		t.Setenv("DATABASE_URL", url)
		_, err := Load()
		if err == nil {
			t.Fatalf("Load: want error for DATABASE_URL=%q", url)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the URL: %v", err)
		}
	}
}

func TestValidSessionID(t *testing.T) {
	t.Parallel()
	good := []string{"a", "default", "alice-1", "team_b", "0", strings.Repeat("a", 63)}
	bad := []string{"", "-a", "_a", "A", "a.b", "a/b", "..", ".", "a b", strings.Repeat("a", 64), "é"}
	for _, id := range good {
		if !ValidSessionID(id) {
			t.Errorf("ValidSessionID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if ValidSessionID(id) {
			t.Errorf("ValidSessionID(%q) = true, want false", id)
		}
	}
}

// TestLoad_HTTPAnyMTLSFatal pins that no MTLS_* combination can start an HTTP
// server. Nothing in the binary builds a tls.Config, so every one of these once
// meant "no TLS" and started a plaintext listener that the operator believed
// was mutually authenticated.
func TestLoad_HTTPAnyMTLSFatal(t *testing.T) {
	cases := []struct {
		name      string
		ca, c, k  string
		token     string
		mustMatch []string
	}{
		{"only CA", "/ca", "", "", "", []string{"MTLS_CA_FILE", "not implemented"}},
		{"only cert", "", "/cert", "", "", []string{"MTLS_CERT_FILE", "not implemented"}},
		{"only key", "", "", "/key", "", []string{"MTLS_KEY_FILE", "not implemented"}},
		{"CA+cert", "/ca", "/cert", "", "", []string{"MTLS_CA_FILE", "MTLS_CERT_FILE"}},
		{"full trio", "/ca", "/cert", "/key", "", []string{"MTLS_CA_FILE", "MTLS_CERT_FILE", "MTLS_KEY_FILE"}},
		{"full trio with admin token", "/ca", "/cert", "/key", "secret", []string{"MTLS_KEY_FILE", "not implemented"}},
		{"partial with admin token", "/ca", "", "", "secret", []string{"MTLS_CA_FILE", "not implemented"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TRANSPORT", "http")
			t.Setenv("ADMIN_TOKEN", tc.token)
			t.Setenv("MTLS_CA_FILE", tc.ca)
			t.Setenv("MTLS_CERT_FILE", tc.c)
			t.Setenv("MTLS_KEY_FILE", tc.k)

			_, err := Load()
			if err == nil {
				t.Fatal("Load: want error, got nil")
			}
			for _, want := range tc.mustMatch {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should contain %q", err, want)
				}
			}
		})
	}
}

// TestLoad_StdioIgnoresMTLS records that the rejection is scoped to the HTTP
// listener. Stdio has no socket, so there is no transport security to be wrong
// about.
func TestLoad_StdioIgnoresMTLS(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("MTLS_CA_FILE", "/run/secrets/ca.pem")
	t.Setenv("MTLS_CERT_FILE", "/run/secrets/cert.pem")
	t.Setenv("MTLS_KEY_FILE", "/run/secrets/key.pem")

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoad_StdioNoAuthRequired(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	// No ADMIN_TOKEN, no MTLS — must succeed.
	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoad_InvalidTransport(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "tcp")

	_, err := Load()
	if err == nil {
		t.Fatal("Load: want error")
	}
	if !strings.Contains(err.Error(), "TRANSPORT") {
		t.Errorf("error %q should mention TRANSPORT", err)
	}
}

func TestLoad_InvalidPort(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("PORT", "not-a-number")

	if _, err := Load(); err == nil {
		t.Fatal("Load: want error for non-numeric PORT")
	}
}

func TestLoad_PortOutOfRange(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("PORT", "70000")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PORT") {
		t.Fatalf("want PORT range error, got %v", err)
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("want LOG_LEVEL error, got %v", err)
	}
}

func TestLoad_InvalidLogFormat(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("LOG_FORMAT", "xml")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Fatalf("want LOG_FORMAT error, got %v", err)
	}
}

func TestLoad_EnablePprofParsed(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("ENABLE_PPROF", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EnablePprof {
		t.Error("EnablePprof = false, want true")
	}
}

func TestLoad_EnablePprofInvalid(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("ENABLE_PPROF", "yeahmaybe")

	if _, err := Load(); err == nil {
		t.Fatal("want parse error for ENABLE_PPROF")
	}
}

func TestLoad_TransportCaseInsensitive(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "HTTP")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transport != TransportHTTP {
		t.Errorf("Transport = %q, want %q", cfg.Transport, TransportHTTP)
	}
}

func TestLoad_EmptyDataDir(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("DATA_DIR", "")
	// DATA_DIR="" falls back to /data default via getEnv, so this still passes.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir = %q, want /data (default)", cfg.DataDir)
	}
}

func TestLoad_MediaRetentionDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("DATA_DIR", "/data")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The media store is a re-downloadable cache, so it ships capped
	// rather than growing until the volume fills.
	if cfg.MediaMaxBytes != DefaultMediaMaxBytes {
		t.Errorf("MediaMaxBytes = %d, want %d", cfg.MediaMaxBytes, DefaultMediaMaxBytes)
	}
	// Age-based eviction is opt-in: deleting media a user can still see in
	// their chat history should be a deliberate choice.
	if cfg.MediaTTL != 0 {
		t.Errorf("MediaTTL = %s, want 0 (disabled)", cfg.MediaTTL)
	}
	if cfg.MediaMaxUploadBytes != DefaultMediaMaxUploadBytes {
		t.Errorf("MediaMaxUploadBytes = %d, want %d", cfg.MediaMaxUploadBytes, DefaultMediaMaxUploadBytes)
	}
	if cfg.MediaSweepInterval != time.Hour {
		t.Errorf("MediaSweepInterval = %s, want 1h", cfg.MediaSweepInterval)
	}
}

func TestLoad_MediaRetentionOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("DATA_DIR", "/srv/wa")
	t.Setenv("MEDIA_MAX_BYTES", "5368709120")
	t.Setenv("MEDIA_TTL", "168h")
	t.Setenv("MEDIA_SWEEP_INTERVAL", "15m")
	t.Setenv("MEDIA_MAX_UPLOAD_BYTES", "26214400")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MediaMaxBytes != 5368709120 {
		t.Errorf("MediaMaxBytes = %d", cfg.MediaMaxBytes)
	}
	if cfg.MediaTTL != 168*time.Hour {
		t.Errorf("MediaTTL = %s", cfg.MediaTTL)
	}
	if cfg.MediaSweepInterval != 15*time.Minute {
		t.Errorf("MediaSweepInterval = %s", cfg.MediaSweepInterval)
	}
	if cfg.MediaMaxUploadBytes != 26214400 {
		t.Errorf("MediaMaxUploadBytes = %d", cfg.MediaMaxUploadBytes)
	}
}

func TestLoad_MediaRetentionRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"unparseable size", map[string]string{"MEDIA_MAX_BYTES": "1GB"}},
		{"negative size", map[string]string{"MEDIA_MAX_BYTES": "-1"}},
		{"unparseable ttl", map[string]string{"MEDIA_TTL": "7 days"}},
		{"negative ttl", map[string]string{"MEDIA_TTL": "-1h"}},
		{"negative interval", map[string]string{"MEDIA_SWEEP_INTERVAL": "-5m"}},
		{"unparseable upload cap", map[string]string{"MEDIA_MAX_UPLOAD_BYTES": "100MiB"}},
		{"negative upload cap", map[string]string{"MEDIA_MAX_UPLOAD_BYTES": "-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TRANSPORT", "stdio")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load succeeded, want an error")
			}
		})
	}
}

// MEDIA_MAX_BYTES=0 is the documented way to disable the cap entirely.
func TestLoad_MediaMaxBytesZeroIsUnlimited(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("MEDIA_MAX_BYTES", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MediaMaxBytes != 0 {
		t.Errorf("MediaMaxBytes = %d, want 0", cfg.MediaMaxBytes)
	}
}
