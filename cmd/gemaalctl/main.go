// Command gemaalctl is the human/CI face of gemaal. G1 skeleton: version
// and the ConnectRPC client commands (plan, checkout, extend). Tenant
// resolution and install labeling arrive with G2 (the package move) — the
// CLI deliberately carries no identity logic yet.
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

func newCommand() *cli.Command {
	return &cli.Command{
		Name:    "gemaalctl",
		Usage:   "talk to the gemaal service: plan, checkout, extend",
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
				Usage: "claim a tenant slot, stamping keep-until (stub until G3 lands server-side)",
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
				Usage: "push a tenant's keep-until forward (stub until G3 lands server-side)",
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
		},
	}
}
