// Command gemaal runs the gemaal service: the pump behind a shared test
// cluster. G3: the housekeeping loop over the label ledger, the six
// RPCs, and the web panel — dry-run (shadow mode) by default.
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

	"github.com/truvity/gemaal/pkg/authn"
	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/engine"
	"github.com/truvity/gemaal/pkg/panel"
	"github.com/truvity/gemaal/pkg/service"
)

// Version is injected at release time by goreleaser's ldflags.
var Version = "dev"

// historyCapacity bounds the in-memory sweep scrollback the panel shows.
const historyCapacity = 200

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

// wire assembles the engine, the service and the panel from the config —
// the single hand-rolled wiring function, no container.
func wire(ctx context.Context, cfg *config.Config, log *slog.Logger) (*service.Service, *panel.Panel, *engine.Engine, error) {
	execClient := engine.ExecClient{Kubecontext: cfg.Kubecontext}

	eng := &engine.Engine{
		Config: cfg,
		Kube:   execClient,
		Helm:   execClient,
		Logger: log,
	}

	if len(cfg.AllowList.SSMRoots) > 0 {
		ssmClient, err := engine.NewAWSSSMClient(ctx, cfg.AWSRegion)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("ssm client: %w", err)
		}

		eng.SSM = ssmClient
	}

	if len(cfg.AllowList.S3Buckets) > 0 {
		s3Client, err := engine.NewAWSS3Client(ctx, cfg.AWSRegion)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("s3 client: %w", err)
		}

		eng.S3 = s3Client
	}

	// TokenReview needs the cluster; outside one (local development) the
	// authenticator falls back to gateway-JWT parsing alone.
	var reviewer authn.TokenReviewer

	kubeReviewer, err := authn.NewInClusterTokenReviewer(cfg.Authz.TokenAudiences)

	switch {
	case err == nil:
		reviewer = kubeReviewer
	case errors.Is(err, authn.ErrNotInCluster):
		log.Warn("not in a cluster: TokenReview disabled, workload tokens will not authenticate")
	default:
		return nil, nil, nil, fmt.Errorf("token reviewer: %w", err)
	}

	auth := &authn.Authenticator{Reviewer: reviewer, GroupsClaim: cfg.Authz.GroupsClaim}
	if cfg.Authz.UserinfoURL != "" {
		auth.Enricher = &authn.UserinfoEnricher{URL: cfg.Authz.UserinfoURL}
	}
	history := engine.NewHistory(historyCapacity)

	svc := service.New(service.Deps{
		Config:  cfg,
		Logger:  log,
		Keeper:  eng,
		Auth:    auth,
		History: history,
	})

	ui, err := panel.New(cfg, log, svc, Version)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("panel: %w", err)
	}

	return svc, ui, eng, nil
}

// tick is one housekeeping pass: plan, then apply (which records
// without deleting when dry-run). A failing tick only ever DELAYS
// cleanup — it is logged and the next tick starts fresh; nothing
// carries over because there is no state but the cluster.
func tick(ctx context.Context, cfg *config.Config, eng *engine.Engine, history *engine.History, log *slog.Logger) {
	plan, err := eng.Plan(ctx, engine.Narrow{})
	if err != nil {
		log.ErrorContext(ctx, "housekeeping tick failed planning", slog.Any("error", err))

		return
	}

	record, err := eng.Apply(ctx, plan, !*cfg.DryRun, "loop")
	if record != nil {
		// Record, not Add: consecutive quiet ticks collapse into one
		// history line so the bounded buffer keeps the real sweeps.
		history.Record(*record)
	}

	if err != nil {
		log.ErrorContext(ctx, "housekeeping tick failed applying", slog.Any("error", err))
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

	svc, ui, eng, err := wire(ctx, cfg, log)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	path, handler := svc.Handler()
	mux.Handle(path, handler)
	ui.Register(mux)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// The housekeeping loop: level-triggered, every tick re-derives the
	// world from what exists. A broken loop only ever DELAYS cleanup.
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return fmt.Errorf("create scheduler: %w", err)
	}

	_, err = scheduler.NewJob(
		gocron.DurationJob(cfg.HousekeepingInterval.Std()),
		gocron.NewTask(func() {
			tick(ctx, cfg, eng, svc.History(), log)
		}),
		// One tick at a time: a slow uninstall must not stack ticks.
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
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
