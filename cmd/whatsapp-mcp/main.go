// Command whatsapp-mcp is the binary entry point: it loads config from env,
// constructs the Server, and runs it until SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/server"
)

var version = "0.0.0-dev"

const usage = `whatsapp-mcp — WhatsApp MCP server (single-container)

Usage:
  whatsapp-mcp                Run the server using TRANSPORT/PORT env vars.
  whatsapp-mcp --healthcheck  Probe http://127.0.0.1:$PORT/healthz and exit.
                              Exits 0 on 200 OK, 1 otherwise. Intended for the
                              Docker HEALTHCHECK on distroless images, which
                              have no shell or curl.
  whatsapp-mcp --version      Print the build version and exit.
  whatsapp-mcp --help         Print this message and exit.

See REQUIREMENTS.md and README.md for the full environment-variable contract.
`

func main() {
	err := dispatch(os.Args[1:], os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
	os.Exit(1)
}

func dispatch(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("whatsapp-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	var (
		showVersion bool
		healthcheck bool
	)
	fs.BoolVar(&showVersion, "version", false, "print version and exit")
	fs.BoolVar(&healthcheck, "healthcheck", false, "probe /healthz and exit")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if showVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}

	if healthcheck {
		return runHealthcheck()
	}

	if fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}

	return run()
}

func run() error {
	server.Version = version

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger, err := applog.New(applog.Options{
		Level:  cfg.LogLevel,
		Format: cfg.LogFormat,
	})
	if err != nil {
		return fmt.Errorf("log: %w", err)
	}

	applog.WithEvent(logger, "app.boot").Info("whatsapp-mcp starting", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return server.New(cfg, logger).Run(ctx)
}

// runHealthcheck probes the unauthenticated /healthz endpoint on the loopback
// interface. The exit code drives the Docker HEALTHCHECK on distroless images
// where no shell or curl is available. The port mirrors config.Load's PORT
// handling (default 8081) so the probe tracks the server's bind port.
//
// /healthz reports WhatsApp readiness, not just process liveness, so this
// exits non-zero when the device is linked but the socket is down — the
// state that would otherwise leave a container reporting healthy while
// every tool call fails. An unpaired container still passes: it is waiting
// on a human, not broken.
func runHealthcheck() error {
	port := 8081
	if v := os.Getenv("PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid PORT %q", v)
		}
		port = n
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	url := "http://" + addr + "/healthz"

	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: unexpected status %d", resp.StatusCode)
	}
	return nil
}
