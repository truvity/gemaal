package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ErrOutOfReach is returned when an action addresses something outside
// the configured reach. Apply fails the whole plan on it: a plan that
// wants to touch something the config does not cover is not a plan
// worth half-executing.
var ErrOutOfReach = errors.New("engine: target is outside the configured reach")

type (
	// ActionResult is the structured deletion record for one action:
	// what was (or would have been) removed, why, and what happened.
	ActionResult struct {
		Action Action `json:"action"`
		// Executed is true when the deletion actually happened; false on
		// dry runs and errors.
		Executed bool `json:"executed"`
		// Deleted counts the underlying objects removed.
		Deleted int `json:"deleted"`
		// Err is set when the action was attempted and failed.
		Err string `json:"error,omitempty"`
	}

	// SweepRecord is the audit record of one sweep execution — one loop
	// tick or one Sweep RPC. The panel's history shows these.
	SweepRecord struct {
		At time.Time `json:"at"`
		// DryRun records the effective mode: true = nothing was deleted.
		DryRun bool `json:"dry_run"`
		// Source says which face ran the sweep: "loop" or "rpc".
		Source string `json:"source"`
		// Results holds one record per planned action.
		Results []ActionResult `json:"results"`
		// Problems carries the plan's enumeration problems forward.
		Problems []TargetProblem `json:"problems,omitempty"`
		// Kept counts the items considered and spared.
		Kept int `json:"kept"`
	}
)

// Apply executes a Plan (or dry-runs it — confirm=false records every
// action as not executed and touches nothing; that the default is
// plan-only lives in the config's dryRun, which the callers translate).
//
// Three gates stand between a Plan and a deletion:
//
//  1. the config is validated structurally at load; its reach is
//     re-derived here, not trusted from the plan;
//  2. confirm must be true;
//  3. every action is re-authorized against the configured reach before
//     ANY deletion. One unauthorized action fails the whole plan.
func (e *Engine) Apply(ctx context.Context, plan *Plan, confirm bool, source string) (*SweepRecord, error) {
	record := &SweepRecord{
		At:       e.now(),
		DryRun:   !confirm,
		Source:   source,
		Problems: plan.Problems,
		Kept:     len(plan.Keep),
	}

	for i := range plan.Delete {
		if err := e.authorize(plan.Delete[i], plan); err != nil {
			return record, err
		}
	}

	actions := make([]Action, len(plan.Delete))
	copy(actions, plan.Delete)
	sortActions(actions)

	if !confirm {
		for i := range actions {
			record.Results = append(record.Results, ActionResult{Action: actions[i], Executed: false})
			e.logDeletion(ctx, actions[i], ActionResult{Action: actions[i]}, true)
		}

		return record, nil
	}

	var errs []error

	for i := range actions {
		action := actions[i]

		deleted, err := e.execute(ctx, action)

		result := ActionResult{Action: action, Executed: err == nil, Deleted: deleted}
		if err != nil {
			result.Err = err.Error()

			errs = append(errs, fmt.Errorf("%s %s: %w", action.Item.Kind, action.Item.ID, err))
		}

		record.Results = append(record.Results, result)
		e.logDeletion(ctx, action, result, false)
	}

	return record, errors.Join(errs...)
}

// authorize re-derives reachability from the configuration and this
// run's tier-selected namespace set. A plan may be stale by the time it
// is applied; authorization means the reach — not the plan — decides
// what is touchable.
func (e *Engine) authorize(action Action, plan *Plan) error {
	switch action.Item.Kind {
	case KindHelmRelease:
		if !e.tierSelected(action.Item.Tenant.Namespace, plan) {
			return fmt.Errorf("%w: namespace %q is not tier-selected", ErrOutOfReach, action.Item.Tenant.Namespace)
		}

		want := action.Item.Tenant.Namespace + "/"
		if !strings.HasPrefix(action.Item.ID, want) {
			return fmt.Errorf("%w: release %q is not in %q", ErrOutOfReach, action.Item.ID, want)
		}

		if installOf(strings.TrimPrefix(action.Item.ID, want)) != action.Item.Tenant.Release {
			return fmt.Errorf("%w: release %q does not belong to tenant %s", ErrOutOfReach, action.Item.ID, action.Item.Tenant)
		}

		return nil

	case KindSSMPrefix:
		want := ""

		for _, root := range e.Config.AllowList.SSMRoots {
			root = normalizedRoot(root)
			if action.Item.ID == fmt.Sprintf("%s%s/%s/", root, action.Item.Tenant.Namespace, action.Item.Tenant.Release) {
				want = action.Item.ID

				break
			}
		}

		if want == "" {
			return fmt.Errorf("%w: %q is not a tenant subtree under an allow-listed ssm root", ErrOutOfReach, action.Item.ID)
		}

		for _, name := range action.Item.Names {
			if !strings.HasPrefix(name, want) {
				return fmt.Errorf("%w: parameter %q is outside %q", ErrOutOfReach, name, want)
			}
		}

		return nil

	case KindS3Prefix:
		for _, bucket := range e.Config.AllowList.S3Buckets {
			if action.Item.ID == fmt.Sprintf("s3://%s/%s/%s/", bucket, action.Item.Tenant.Namespace, action.Item.Tenant.Release) {
				return nil
			}
		}

		return fmt.Errorf("%w: %q is not a tenant prefix in an allow-listed bucket", ErrOutOfReach, action.Item.ID)

	default:
		return fmt.Errorf("%w: unknown item kind %q", ErrOutOfReach, action.Item.Kind)
	}
}

func (e *Engine) execute(ctx context.Context, action Action) (int, error) {
	switch action.Item.Kind {
	case KindHelmRelease:
		release, found := strings.CutPrefix(action.Item.ID, action.Item.Tenant.Namespace+"/")
		if !found {
			return 0, fmt.Errorf("cannot derive release name from %q", action.Item.ID)
		}

		if err := e.Helm.Uninstall(ctx, action.Item.Tenant.Namespace, release); err != nil {
			return 0, err
		}

		return 1, nil

	case KindSSMPrefix:
		if e.SSM == nil {
			return 0, errors.New("no ssm client configured")
		}

		if err := e.SSM.DeleteParameters(ctx, action.Item.Names); err != nil {
			return 0, err
		}

		return len(action.Item.Names), nil

	case KindS3Prefix:
		if e.S3 == nil {
			return 0, errors.New("no s3 client configured")
		}

		bucket, rest, ok := strings.Cut(strings.TrimPrefix(action.Item.ID, "s3://"), "/")
		if !ok {
			return 0, fmt.Errorf("cannot derive bucket from %q", action.Item.ID)
		}

		return e.S3.DeletePrefix(ctx, bucket, rest)

	default:
		return 0, fmt.Errorf("unknown item kind %q", action.Item.Kind)
	}
}

// logDeletion emits the structured record required of every destructive
// (or would-be destructive) action: what, which tenant, how old, which
// rule matched, and what happened.
func (e *Engine) logDeletion(ctx context.Context, action Action, result ActionResult, dryRun bool) {
	attrs := []any{
		slog.String("kind", string(action.Item.Kind)),
		slog.String("id", action.Item.ID),
		slog.String("tenant_namespace", action.Item.Tenant.Namespace),
		slog.String("tenant_release", action.Item.Tenant.Release),
		slog.Duration("age", roundAge(action.Item.Age)),
		slog.String("rule", string(action.Rule)),
		slog.String("reason", action.Reason),
		slog.Bool("dry_run", dryRun),
		slog.Int("deleted", result.Deleted),
	}

	switch {
	case dryRun:
		e.log().InfoContext(ctx, "housekeeping would delete", attrs...)
	case result.Err != "":
		e.log().ErrorContext(ctx, "housekeeping deletion failed", append(attrs, slog.String("error", result.Err))...)
	default:
		e.log().InfoContext(ctx, "housekeeping deleted", attrs...)
	}
}
