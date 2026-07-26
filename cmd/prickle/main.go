// SPDX-License-Identifier: Apache-2.0

// Command prickle is a Prometheus exporter for host, container and GPU metrics.
//
// See SPEC.md for the contract this implements. Phase 1 ships the host
// collector; `prickle diagnose` reports what the exporter can and cannot read.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/collector"
	"github.com/starkdrift/prickle-exporter/internal/collector/host"
	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/sampler"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "prickle:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// One subcommand. Anything else is a flag on the exporter itself.
	if len(args) > 0 && args[0] == "diagnose" {
		return diagnose(args[1:], os.Stdout)
	}

	fs := flag.NewFlagSet("prickle", flag.ContinueOnError)
	var cfg config
	cfg.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.showVersion {
		fmt.Printf("prickle %s (%s)\n", version, runtime.Version())
		return nil
	}

	log := cfg.logger()

	node, err := cfg.nodeName()
	if err != nil {
		return err
	}
	hostOpts, err := cfg.hostOptions()
	if err != nil {
		return err
	}

	collectors := []collector.Collector{host.New(hostOpts)}

	s := sampler.New(collectors, sampler.Options{
		Interval:    cfg.interval,
		Timeout:     cfg.timeout,
		ConstLabels: []exposition.Label{exposition.L("node", node)},
		Version:     version,
		Logger:      log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go s.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle(cfg.telemetryPath, s)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, landingPage, cfg.telemetryPath, cfg.telemetryPath, version)
	})

	srv := &http.Server{
		Addr:              cfg.listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "address", cfg.listenAddress, "path", cfg.telemetryPath, "node", node)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

const landingPage = `<!doctype html>
<title>prickle</title>
<h1>prickle</h1>
<p><a href="%s">%s</a></p>
<p>version %s</p>
`
