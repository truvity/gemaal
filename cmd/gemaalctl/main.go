// Command gemaalctl is the human/CI face of gemaal.
//
//   - whoami / install / uninstall: tenant resolution (identity evidence
//     chain → email → slug → personal namespace; see pkg/identity and
//     pkg/harness) and client-side helm installs stamped with the gemaal
//     ledger labels. gemaal itself never installs anything — these run
//     on the client, which is exactly the point.
//   - plan / checkout / extend: ConnectRPC clients of the gemaal service.
//   - pipeline (snapshot, push-preview, release-stable): the promoted
//     form of bar's url-shortener release scripts — see pkg/pipeline.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"
	"google.golang.org/protobuf/types/known/durationpb"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
	"github.com/truvity/gemaal/gen/gemaal/v1/gemaalv1connect"
	"github.com/truvity/gemaal/pkg/pipeline"
)

// Version is injected at release time by goreleaser's ldflags.
var Version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := newCommand().Run(ctx, os.Args); err != nil {
		slog.Error("fatal", slog.Any("error", err))

		return 1
	}

	return 0
}

func newClient(server string) gemaalv1connect.GemaalServiceClient {
	return gemaalv1connect.NewGemaalServiceClient(http.DefaultClient, server)
}

func serverFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     "server",
		Usage:    "base URL of the gemaal service, e.g. http://localhost:8080",
		Sources:  cli.EnvVars("GEMAAL_SERVER"),
		Required: true,
	}
}

func pipelineConfigFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     "config",
		Usage:    "per-project pipeline configuration (YAML) — see pipeline.example.yaml",
		Sources:  cli.EnvVars("GEMAAL_PIPELINE_CONFIG"),
		Required: true,
	}
}

// pipelineAction loads the per-project config and hands a wired Pipeline
// to the flow.
func pipelineAction(flow func(context.Context, *pipeline.Pipeline) error) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		cfg, err := pipeline.Load(cmd.String("config"))
		if err != nil {
			return err
		}

		p := pipeline.New(cfg, pipeline.ExecRunner{}, slog.Default(), os.Stderr)

		return flow(ctx, p)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:    "gemaalctl",
		Usage:   "resolve the tenant, install with ledger labels, talk to the gemaal service — plus the artifact pipeline",
		Version: Version,
		Commands: []*cli.Command{
			{
				Name:  "version",
				Usage: "print the build stamp and exit",
				Action: func(context.Context, *cli.Command) error {
					fmt.Println(Version)

					return nil
				},
			},
			whoamiCommand(),
			installCommand(),
			uninstallCommand(),
			{
				Name:  "plan",
				Usage: "ask the service what housekeeping would do right now (no side effects)",
				Flags: []cli.Flag{
					serverFlag(),
					&cli.StringFlag{Name: "namespace", Usage: "narrow the plan to one namespace"},
					&cli.StringFlag{Name: "release", Usage: "narrow the plan to one release"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					client := newClient(cmd.String("server"))

					resp, err := client.Plan(ctx, connect.NewRequest(&gemaalv1.PlanRequest{
						Namespace: cmd.String("namespace"),
						Release:   cmd.String("release"),
					}))
					if err != nil {
						return err
					}

					actions := resp.Msg.GetActions()
					if len(actions) == 0 {
						fmt.Println("plan: nothing to do")

						return nil
					}

					for _, a := range actions {
						fmt.Printf("%s\t%s/%s\t%s\trule=%s\t%s\n",
							a.GetKind(), a.GetNamespace(), a.GetRelease(), a.GetTarget(), a.GetRule(), a.GetReason())
					}

					return nil
				},
			},
			{
				Name:  "checkout",
				Usage: "claim a tenant slot, stamping keep-until",
				Flags: []cli.Flag{
					serverFlag(),
					&cli.StringFlag{Name: "namespace", Usage: "tenant namespace", Required: true},
					&cli.StringFlag{Name: "release", Usage: "tenant release", Required: true},
					&cli.DurationFlag{Name: "for", Usage: "how long to hold the tenant", Value: 8 * time.Hour},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					client := newClient(cmd.String("server"))

					resp, err := client.Checkout(ctx, connect.NewRequest(&gemaalv1.CheckoutRequest{
						Namespace: cmd.String("namespace"),
						Release:   cmd.String("release"),
						Duration:  durationpb.New(cmd.Duration("for")),
					}))
					if err != nil {
						return err
					}

					fmt.Printf("checked out %s/%s until %s\n",
						resp.Msg.GetTenant().GetNamespace(),
						resp.Msg.GetTenant().GetRelease(),
						resp.Msg.GetTenant().GetKeepUntil().AsTime().Format(time.RFC3339))

					return nil
				},
			},
			{
				Name:  "extend",
				Usage: "push a tenant's keep-until forward",
				Flags: []cli.Flag{
					serverFlag(),
					&cli.StringFlag{Name: "namespace", Usage: "tenant namespace", Required: true},
					&cli.StringFlag{Name: "release", Usage: "tenant release", Required: true},
					&cli.DurationFlag{Name: "for", Usage: "how far past now to move keep-until", Value: 8 * time.Hour},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					client := newClient(cmd.String("server"))

					resp, err := client.Extend(ctx, connect.NewRequest(&gemaalv1.ExtendRequest{
						Namespace: cmd.String("namespace"),
						Release:   cmd.String("release"),
						Duration:  durationpb.New(cmd.Duration("for")),
					}))
					if err != nil {
						return err
					}

					fmt.Printf("extended %s/%s until %s\n",
						resp.Msg.GetTenant().GetNamespace(),
						resp.Msg.GetTenant().GetRelease(),
						resp.Msg.GetTenant().GetKeepUntil().AsTime().Format(time.RFC3339))

					return nil
				},
			},
			{
				Name: "decommission",
				Usage: "uninstall a tenant's release ring NOW via the service — the explicit end-of-life call; " +
					"the tenant's own keep-until does not hold it, the server's dry-run setting wins",
				Flags: []cli.Flag{
					serverFlag(),
					&cli.StringFlag{Name: "namespace", Usage: "tenant namespace", Required: true},
					&cli.StringFlag{Name: "release", Usage: "tenant release", Required: true},
					&cli.BoolFlag{Name: "dry-run", Usage: "report what would go without uninstalling"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					client := newClient(cmd.String("server"))

					resp, err := client.Decommission(ctx, connect.NewRequest(&gemaalv1.DecommissionRequest{
						Namespace: cmd.String("namespace"),
						Release:   cmd.String("release"),
						DryRun:    cmd.Bool("dry-run"),
					}))
					if err != nil {
						return err
					}

					mode := "uninstalled"
					if resp.Msg.GetDryRun() {
						mode = "would uninstall"
					}

					for _, r := range resp.Msg.GetResults() {
						line := fmt.Sprintf("%s %s/%s", mode, r.GetAction().GetNamespace(), r.GetAction().GetRelease())
						if r.GetError() != "" {
							line += " — FAILED: " + r.GetError()
						}

						fmt.Println(line)
					}

					if len(resp.Msg.GetResults()) == 0 {
						fmt.Println("nothing to do")
					}

					return nil
				},
			},
			{
				Name:  "pipeline",
				Usage: "artifact pipeline: dev-loop snapshot, preview chart push, gated stable release",
				Commands: []*cli.Command{
					{
						Name: "snapshot",
						Usage: "DEV LOOP build — preview registry only, no knobs, no gates; " +
							"builds the working tree as-is (dirty/uncommitted included)",
						Flags: []cli.Flag{pipelineConfigFlag()},
						Action: pipelineAction(func(ctx context.Context, p *pipeline.Pipeline) error {
							return p.Snapshot(ctx)
						}),
					},
					{
						Name: "push-preview",
						Usage: "push the packaged ring charts to the PREVIEW registry — " +
							"refuses charts not stamped 'preview' and incoherent ring pairs",
						Flags: []cli.Flag{pipelineConfigFlag()},
						Action: pipelineAction(func(ctx context.Context, p *pipeline.Pipeline) error {
							return p.PushPreview(ctx)
						}),
					},
					{
						Name: "release-stable",
						Usage: "STABLE RELEASE — five gates before any build or credential use, " +
							"then build + push to the stable registry in one run",
						Flags: []cli.Flag{pipelineConfigFlag()},
						Action: pipelineAction(func(ctx context.Context, p *pipeline.Pipeline) error {
							return p.ReleaseStable(ctx)
						}),
					},
				},
			},
		},
	}
}
