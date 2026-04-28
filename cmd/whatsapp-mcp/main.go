// Command whatsapp-mcp is the binary entry point: it loads config from env,
// constructs the Server, and runs it until SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/server"
)

var version = "0.0.0-dev"

const usage = `whatsapp-mcp — WhatsApp MCP server (single-container)

Usage:
  whatsapp-mcp            Run the server using TRANSPORT/PORT env vars.
  whatsapp-mcp --version  Print the build version and exit.
  whatsapp-mcp --help     Print this message and exit.

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

	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if showVersion {
		fmt.Fprintln(stdout, version)
		return nil
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
