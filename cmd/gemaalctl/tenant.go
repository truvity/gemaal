package main

// The tenant-facing commands: whoami (identity evidence + resolved
// tenant), install and uninstall (client-side helm with the gemaal
// ledger labels stamped — gemaal itself never installs anything; these
// run on the CLIENT, which is exactly the point).

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/truvity/gemaal/pkg/gemaalcfg"
	"github.com/truvity/gemaal/pkg/harness"
	"github.com/truvity/gemaal/pkg/identity"
)

// newRunner builds the executor for helm, kubectl and build hooks — a
// package var so CLI tests inject a scripted runner.
var newRunner = func() harness.Runner { return harness.ExecRunner{} }

// say writes CLI output, ignoring the writer's error the way fmt.Printf
// does — a broken output pipe is not a command failure.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// tenantFlags are shared by whoami/install/uninstall. gemaal.yaml
// carries the committed defaults; flags (with their GEMAAL_* env
// sources) always win.
func tenantFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "kubecontext",
			Aliases: []string{"c"},
			Usage:   "kubeconfig context name (kubeconfig is the cluster registry)",
			Sources: cli.EnvVars(harness.EnvKubecontext),
		},
		&cli.StringFlag{
			Name:    "kubeconfig",
			Usage:   "override the ambient kubeconfig",
			Sources: cli.EnvVars("GEMAAL_KUBECONFIG"),
		},
		&cli.StringFlag{
			Name:    "namespace",
			Aliases: []string{"n"},
			Usage:   "target namespace (skips identity resolution)",
			Sources: cli.EnvVars(harness.EnvNamespace),
		},
		&cli.StringFlag{
			Name:    "release",
			Aliases: []string{"r"},
			Usage:   "release name — the other half of the tenant identity",
			Sources: cli.EnvVars(harness.EnvRelease),
		},
		&cli.StringFlag{
			Name:    "app",
			Usage:   "application name: the release of a standing employee install",
			Sources: cli.EnvVars("GEMAAL_APP"),
		},
		&cli.StringFlag{
			Name:    "personal-namespace",
			Usage:   "personal namespace template, e.g. emp-{slug}",
			Sources: cli.EnvVars("GEMAAL_PERSONAL_NAMESPACE"),
		},
		&cli.StringFlag{
			Name:    "config",
			Usage:   "gemaal.yaml path (optional file)",
			Value:   gemaalcfg.DefaultPath,
			Sources: cli.EnvVars("GEMAAL_CONFIG"),
		},
	}
}

// tenantOptions builds the resolution input shared by the tenant
// commands: flags (which already carry their GEMAAL_* env sources) over
// gemaal.yaml. No identity map in the config is fine — the resolver
// ladder (harness.Options.SlugResolver) falls through to the interim
// kubectl-groups resolver, run through the CLI's injectable runner.
func tenantOptions(cmd *cli.Command) (harness.Options, *gemaalcfg.Config, error) {
	cfg, err := gemaalcfg.Load(cmd.String("config"))
	if err != nil {
		return harness.Options{}, nil, err
	}

	return harness.Options{
		Kubecontext:       cmd.String("kubecontext"),
		Kubeconfig:        cmd.String("kubeconfig"),
		Namespace:         cmd.String("namespace"),
		Release:           cmd.String("release"),
		App:               cmd.String("app"),
		PersonalNamespace: cmd.String("personal-namespace"),
		Config:            cfg,
		IdentityRunner:    newRunner(),
	}, cfg, nil
}

func whoamiCommand() *cli.Command {
	return &cli.Command{
		Name:  "whoami",
		Usage: "the identity evidence chain + the resolved tenant",
		Flags: tenantFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, _, err := tenantOptions(cmd)
			if err != nil {
				return err
			}

			w := cmd.Root().Writer

			// The evidence walk, driver by driver — whoami exists to show
			// the trail, not just the answer.
			res, resErr := identity.DefaultChain(opts.Kubecontext, opts.Kubeconfig).Resolve(ctx)

			for _, a := range res.Attempts {
				if a.Err != nil {
					say(w, "evidence:  %s: (%v)\n", a.Driver, a.Err)

					continue
				}

				say(w, "evidence:  %s: %s\n", a.Driver, a.Email)
			}

			if resErr != nil {
				say(w, "email:     (unresolved: %v)\n", resErr)
			} else {
				say(w, "email:     %s (%s)\n", res.Email, res.Driver)
				printSlug(ctx, w, opts, res.Email)

				// Reuse the evidence already gathered: one chain walk, and
				// the tenant printed below is exactly what install would use.
				opts.Chain = identity.Chain{identity.Static(res.Email)}
			}

			// The two halves print independently: a missing release must
			// not hide the namespace, which is what whoami is usually for.
			overridden := opts.Namespace != "" || os.Getenv(harness.EnvNamespace) != ""

			if resErr != nil && !overridden {
				say(w, "namespace: (unresolved: no identity evidence — see above)\n")
			} else if ns, nsErr := harness.ResolveNamespace(ctx, opts); nsErr != nil {
				say(w, "namespace: (unresolved: %v)\n", nsErr)
			} else {
				say(w, "namespace: %s\n", ns)
				say(w, "tier:      %s\n", harness.TierForNamespace(ns))
			}

			if release, relErr := harness.ResolveRelease(opts); relErr != nil {
				say(w, "release:   (unresolved: %v)\n", relErr)
			} else {
				say(w, "release:   %s\n", release)
			}

			return nil
		},
	}
}

// printSlug shows the resolver's view of the caller, without failing
// whoami: an unmapped email is a finding, not a crash. The resolver is
// the exact one install uses (harness.Options.SlugResolver): the
// gemaal.yaml identity map when one is committed, else the interim
// kubectl-groups resolver reading the caller's own emp:{slug} token
// group — no identity config file required.
func printSlug(ctx context.Context, w io.Writer, opts harness.Options, email string) {
	resolver, err := opts.SlugResolver()
	if err != nil {
		say(w, "slug:      (identity map: %v)\n", err)

		return
	}

	if slug, slugErr := resolver.Resolve(ctx, email); slugErr != nil {
		say(w, "slug:      (unresolved: %v)\n", slugErr)
	} else {
		say(w, "slug:      %s\n", slug)
	}
}

func installCommand() *cli.Command {
	return &cli.Command{
		Name: "install",
		Usage: "helm upgrade --install into the resolved tenant, stamping the gemaal ledger labels " +
			"(with --infra-chart: the ring pair, infra first)",
		Flags: append(tenantFlags(),
			&cli.StringFlag{
				Name:     "chart",
				Usage:    "application (ring3) chart: local path, packaged .tgz, or OCI ref",
				Required: true,
			},
			&cli.StringFlag{
				Name:  "infra-chart",
				Usage: "infrastructure (ring2) chart — installs <release>-infra BEFORE <release>",
			},
			&cli.StringFlag{
				Name:  "ttl",
				Usage: "ledger ttl label, a Go duration like 24h (unset: the namespace tier's default applies)",
			},
			&cli.StringFlag{
				Name:  "keep-until",
				Usage: "ledger keep-until label, RFC3339 (e.g. 2026-08-05T18:00:00Z)",
			},
			&cli.StringFlag{
				Name:  "execution-id",
				Usage: "ledger execution-id label (default: CI run coordinates, or a local timestamp)",
			},
			&cli.StringSliceFlag{
				Name:    "values",
				Aliases: []string{"f"},
				Usage:   "passed to helm --values, after the gemaal.yaml cluster values (later wins)",
			},
			&cli.StringSliceFlag{
				Name:  "set",
				Usage: "passed through to helm --set",
			},
			&cli.BoolFlag{
				Name:  "build",
				Usage: "run the gemaal.yaml build hook first, with the resolved GEMAAL_* env exported",
			},
			&cli.StringFlag{
				Name:    "project",
				Usage:   "project name for per-project gemaal.yaml hooks (default: --app)",
				Sources: cli.EnvVars("GEMAAL_PROJECT"),
			},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, cfg, err := tenantOptions(cmd)
			if err != nil {
				return err
			}

			tenant, err := harness.Resolve(ctx, opts)
			if err != nil {
				return err
			}

			runner := newRunner()

			if cmd.Bool("build") {
				if err := runBuildHook(ctx, cfg, cmd, tenant, opts.Kubecontext, runner); err != nil {
					return err
				}
			}

			labels, err := installLabels(cmd)
			if err != nil {
				return err
			}

			values := cmd.StringSlice("values")

			// The cluster's opaque values ride as .Values.gemaal — FIRST in
			// the flag order, so the caller's -f/--set still win under
			// helm's own precedence.
			if cv := cfg.ClusterValues(opts.Kubecontext); len(cv) > 0 {
				path, cvErr := harness.ClusterValuesFile(cv)
				if cvErr != nil {
					return cvErr
				}

				defer func() { _ = os.Remove(path) }()

				values = append([]string{path}, values...)
			}

			cluster := &harness.Cluster{Kubecontext: opts.Kubecontext, Kubeconfig: opts.Kubeconfig, Runner: runner}

			if infra := cmd.String("infra-chart"); infra != "" {
				err = cluster.InstallPair(ctx, tenant, harness.Pair{
					InfraChart:  infra,
					AppChart:    cmd.String("chart"),
					ValuesFiles: values,
					Set:         cmd.StringSlice("set"),
					Labels:      labels,
				})
			} else {
				err = cluster.Install(ctx, tenant.Namespace, harness.Install{
					Release:     tenant.Release,
					Chart:       cmd.String("chart"),
					ValuesFiles: values,
					Set:         cmd.StringSlice("set"),
					Labels:      labels,
				})
			}

			if err != nil {
				return err
			}

			say(cmd.Root().Writer, "installed %s\n", tenant)

			return nil
		},
	}
}

// installLabels assembles the ledger stamp from the flags, deriving the
// execution id when the caller does not name one.
func installLabels(cmd *cli.Command) (harness.Labels, error) {
	labels := harness.Labels{
		TTL:         cmd.String("ttl"),
		ExecutionID: cmd.String("execution-id"),
	}

	if labels.ExecutionID == "" {
		labels.ExecutionID = harness.DefaultExecutionID(time.Now())
	}

	if ku := cmd.String("keep-until"); ku != "" {
		ts, err := time.Parse(time.RFC3339, ku)
		if err != nil {
			return harness.Labels{}, fmt.Errorf("--keep-until: %w", err)
		}

		labels.KeepUntil = ts
	}

	return labels, nil
}

// runBuildHook executes the repo-owned opaque build argv — the project's
// own hook when gemaal.yaml defines one, else the global hook — with the
// resolved GEMAAL_* env exported.
func runBuildHook(
	ctx context.Context,
	cfg *gemaalcfg.Config,
	cmd *cli.Command,
	tenant harness.Tenant,
	kubecontext string,
	runner harness.Runner,
) error {
	project := cmd.String("project")
	if project == "" {
		project = cmd.String("app")
	}

	hook := cfg.BuildHook(project)
	if len(hook) == 0 {
		return fmt.Errorf("--build: no hooks.build in gemaal.yaml (global or projects.%s.hooks.build)", project)
	}

	// In-process export: the hook (and anything it spawns) inherits the
	// resolved tenant — the bidirectional GEMAAL_* contract.
	if err := tenant.Export(kubecontext); err != nil {
		return err
	}

	return runner.Run(ctx, hook...)
}

func uninstallCommand() *cli.Command {
	return &cli.Command{
		Name: "uninstall",
		Usage: "uninstall the resolved tenant's ring pair — <release> first, then <release>-infra; " +
			"releases that are not there are skipped",
		Flags: tenantFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, _, err := tenantOptions(cmd)
			if err != nil {
				return err
			}

			tenant, err := harness.Resolve(ctx, opts)
			if err != nil {
				return err
			}

			cluster := &harness.Cluster{Kubecontext: opts.Kubecontext, Kubeconfig: opts.Kubeconfig, Runner: newRunner()}

			if err := cluster.UninstallPair(ctx, tenant); err != nil {
				return err
			}

			say(cmd.Root().Writer, "uninstalled %s (and its -infra companion, where present)\n", tenant)

			return nil
		},
	}
}
