// Command cursor-relay lets Cursor's own clients (cursor-agent, @cursor/sdk) use one account they hold no
// credential for: a client presents a key of the operator's gateway, and the relay forwards its calls to
// Cursor with the account's credential. See the README for the configuration.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mediavee/cpa-cursor/internal/relay"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("cursor-relay stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := relay.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	r, err := relay.New(cfg, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	warm, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := r.Warm(warm); err != nil {
		log.Warn("no upstream token yet, retrying on demand", "error", err.Error())
	}
	cancel()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	log.Info("cursor-relay listening", "addr", ln.Addr().String(), "public_url", cfg.PublicURL)
	if err := r.Serve(ctx, ln); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
