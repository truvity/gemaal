// Command gemaal runs the gemaal service: the pump behind a shared test
// cluster. G1 skeleton — stub RPCs, a no-op housekeeping tick, and a
// healthz endpoint; the real engine arrives with G3.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/urfave/cli/v3"

	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/service"
)

// Version is injected at release time by goreleaser's ldflags.
var Version = "dev"

func main() {
	// os.Exit skips deferred calls, so the whole body lives in run() and
	// main does nothing but translate an error into an exit code.
	os.Exit(run())
}

func run() int {
	// SIGTERM is what Kubernetes sends; SIGINT is what a laptop sends.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := newCommand().Run(ctx, os.Args); err != nil {
		slog.Error("fatal", slog.Any("error", err))

		return 1
	}

	return 0
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:    "gemaal",
		Usage:   "the pump behind a shared test cluster: watches, ages, and drains unused test installations",
		Version: Version,
		Flags: []cli.Flag{
			// Not Required: a required root flag would break `gemaal version`.
			// serve() refuses an empty path instead.
			&cli.StringFlag{
				Name:    "config",
				Usage:   "path to the configuration document",
				Sources: cli.EnvVars("GEMAAL_CONFIG"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return serve(ctx, cmd.String("config"))
		},
		Commands: []*cli.Command{
			{
				Name:  "version",
				Usage: "print the build stamp and exit",
				Action: func(context.Context, *cli.Command) error {
					fmt.Println(Version)

					return nil
				},
			},
		},
	}
}

func serve(ctx context.Context, configPath string) error {
	if configPath == "" {
		return errors.New("--config is required (or set GEMAAL_CONFIG)")
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	svc := service.New(cfg, log)

	mux := http.NewServeMux()
	path, handler := svc.Handler()
	mux.Handle(path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// The housekeeping loop. G1: a heartbeat that does nothing — but the
	// scheduler wiring, and the rule that a broken loop only ever DELAYS
	// cleanup, are in place from the first commit.
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return fmt.Errorf("create scheduler: %w", err)
	}

	_, err = scheduler.NewJob(
		gocron.DurationJob(cfg.HousekeepingInterval.Std()),
		gocron.NewTask(func() {
			log.Info("housekeeping tick (no-op until G3)",
				slog.Bool("dry_run", *cfg.DryRun),
				slog.String("label_domain", cfg.LabelDomain),
			)
		}),
	)
	if err != nil {
		return fmt.Errorf("schedule housekeeping: %w", err)
	}

	scheduler.Start()

	defer func() {
		if err := scheduler.Shutdown(); err != nil {
			log.Warn("scheduler shutdown", slog.Any("error", err))
		}
	}()

	// Unencrypted HTTP/2 alongside HTTP/1.1: ConnectRPC serves gRPC
	// clients over cleartext behind an in-cluster gateway that terminates
	// TLS. (Native Protocols field — the old x/net h2c wrapper is
	// deprecated.)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)

	go func() {
		log.Info("gemaal listening",
			slog.String("addr", cfg.Listen),
			slog.String("rpc_path", path),
			slog.Bool("dry_run", *cfg.DryRun),
			slog.String("version", Version),
		)

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return server.Shutdown(shutdownCtx)
}
