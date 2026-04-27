package config

import (
	"strings"
	"testing"
)

// allVars lists every env var the loader reads. Each test clears them before
// setting only what it wants, so tests do not leak values across cases.
var allVars = []string{
	"TRANSPORT", "BIND_ADDR", "PORT", "ADMIN_PORT", "DATA_DIR",
	"LOG_LEVEL", "LOG_FORMAT", "AUTH_TOKEN",
	"MTLS_CA_FILE", "MTLS_CERT_FILE", "MTLS_KEY_FILE",
	"WHATSAPP_DEVICE_NAME", "FFMPEG_PATH", "ENABLE_PPROF",
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
	if cfg.AdminPort != 8082 {
		t.Errorf("AdminPort = %d, want 8082", cfg.AdminPort)
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

func TestLoad_HTTPWithAuthToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "http")
	t.Setenv("AUTH_TOKEN", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthToken != "secret" {
		t.Errorf("AuthToken = %q", cfg.AuthToken)
	}
}

func TestLoad_HTTPMissingAuthFatal(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "http")

	_, err := Load()
	if err == nil {
		t.Fatal("Load: want error, got nil")
	}
	if !strings.Contains(err.Error(), "AUTH_TOKEN") {
		t.Errorf("error %q should mention AUTH_TOKEN", err)
	}
}

func TestLoad_HTTPFullMTLSOK(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "http")
	t.Setenv("MTLS_CA_FILE", "/run/secrets/ca.pem")
	t.Setenv("MTLS_CERT_FILE", "/run/secrets/cert.pem")
	t.Setenv("MTLS_KEY_FILE", "/run/secrets/key.pem")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MTLSEnabled() {
		t.Errorf("MTLSEnabled = false, want true")
	}
}

func TestLoad_HTTPPartialMTLSFatal(t *testing.T) {
	cases := []struct {
		name      string
		ca, c, k  string
		mustMatch string
	}{
		{"only CA", "/ca", "", "", "MTLS_"},
		{"only cert", "", "/cert", "", "MTLS_"},
		{"only key", "", "", "/key", "MTLS_"},
		{"CA+cert", "/ca", "/cert", "", "MTLS_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TRANSPORT", "http")
			t.Setenv("AUTH_TOKEN", "") // absent
			t.Setenv("MTLS_CA_FILE", tc.ca)
			t.Setenv("MTLS_CERT_FILE", tc.c)
			t.Setenv("MTLS_KEY_FILE", tc.k)

			_, err := Load()
			if err == nil {
				t.Fatal("Load: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.mustMatch) {
				t.Errorf("error %q should contain %q", err, tc.mustMatch)
			}
		})
	}
}

func TestLoad_StdioNoAuthRequired(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	// No AUTH_TOKEN, no MTLS — must succeed.
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

func TestLoad_PortAdminPortCollision(t *testing.T) {
	clearEnv(t)
	t.Setenv("TRANSPORT", "stdio")
	t.Setenv("PORT", "9000")
	t.Setenv("ADMIN_PORT", "9000")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("want collision error, got %v", err)
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
	t.Setenv("AUTH_TOKEN", "t")

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
