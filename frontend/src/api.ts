import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import type { Duration, Timestamp } from "@bufbuild/protobuf/wkt";
import { GemaalService, ActionKind } from "./gen/gemaal/v1/gemaal_pb.js";

// The typed ConnectRPC (Connect-Web) client — the SPA's only data path, over
// the same service gemaalctl talks to.
const client = createClient(GemaalService, createConnectTransport({ baseUrl: "/" }));

// ---- display helpers -------------------------------------------------------

// dur renders a protobuf Duration as a compact human string (36h → "1d 12h").
export function dur(d?: Duration): string {
  if (!d) return "—";
  let s = Number(d.seconds);
  if (s === 0) return "0s";
  const parts: string[] = [];
  const day = Math.floor(s / 86400); if (day) { parts.push(`${day}d`); s -= day * 86400; }
  const h = Math.floor(s / 3600); if (h) { parts.push(`${h}h`); s -= h * 3600; }
  const m = Math.floor(s / 60); if (m && parts.length < 2) parts.push(`${m}m`);
  if (parts.length === 0) parts.push(`${s}s`);
  return parts.slice(0, 2).join(" ");
}

// ts renders a protobuf Timestamp as a local date-time string.
export function ts(t?: Timestamp): string {
  if (!t) return "—";
  return new Date(Number(t.seconds) * 1000).toISOString().slice(0, 16).replace("T", " ") + " UTC";
}

export function tsDate(t?: Timestamp): Date | null {
  return t ? new Date(Number(t.seconds) * 1000) : null;
}

// seconds builds a Duration message value from whole hours.
export function hours(h: number): { seconds: bigint; nanos: number; $typeName: "google.protobuf.Duration" } {
  return { $typeName: "google.protobuf.Duration", seconds: BigInt(Math.round(h * 3600)), nanos: 0 };
}

export function actionLabel(kind: ActionKind): string {
  switch (kind) {
    case ActionKind.NONE: return "none";
    case ActionKind.TEARDOWN: return "teardown";
    case ActionKind.SWEEP: return "sweep";
    default: return "—";
  }
}

// ---- views -----------------------------------------------------------------

export interface Tenant {
  namespace: string;
  release: string;
  backend: string;
  tier: string;
  age: string;
  ttl: string;
  keepUntil: string;
  expired: boolean;
  executionId: string;
  pending: string;
}

export interface TenantList {
  tenants: Tenant[];
  unreadable: { namespace: string; reason: string }[];
}

export async function listTenants(signal?: AbortSignal): Promise<TenantList> {
  const resp = await client.listTenants({}, { signal });
  const now = Date.now();
  return {
    tenants: resp.tenants.map((t) => {
      const keep = tsDate(t.keepUntil);
      return {
        namespace: t.namespace,
        release: t.release,
        backend: t.backend,
        tier: t.tier,
        age: dur(t.age),
        ttl: dur(t.ttl),
        keepUntil: t.keepUntil ? ts(t.keepUntil) : "",
        expired: keep !== null && keep.getTime() < now,
        executionId: t.executionId,
        pending: actionLabel(t.pendingAction),
      };
    }),
    unreadable: resp.unreadable.map((u) => ({ namespace: u.namespace, reason: u.reason })),
  };
}

export async function checkout(namespace: string, release: string, holdHours: number): Promise<void> {
  await client.checkout({ namespace, release, duration: hours(holdHours) });
}

export async function extend(namespace: string, release: string, holdHours: number): Promise<void> {
  await client.extend({ namespace, release, duration: hours(holdHours) });
}

export interface DecommissionOutcome {
  dryRun: boolean;
  results: { kind: string; target: string; executed: boolean; error?: string }[];
}

export async function decommission(namespace: string, release: string, dryRun: boolean): Promise<DecommissionOutcome> {
  const resp = await client.decommission({ namespace, release, dryRun });
  return {
    dryRun: resp.dryRun,
    results: resp.results.map((r) => ({
      kind: r.action ? actionLabel(r.action.kind) : "—",
      target: r.action?.target || `${r.action?.namespace ?? ""}/${r.action?.release ?? ""}`,
      executed: r.executed,
      error: r.error || undefined,
    })),
  };
}

export interface SweepDetail { kind: string; target: string; rule: string; reason: string; executed: boolean; error?: string }

export interface SweepRow {
  at: string;
  mode: "executed" | "dry-run" | "quiet";
  source: string;
  actions: number;
  executed: number;
  failed: number;
  kept: number;
  problems: number;
  quietTicks: number;
  details: SweepDetail[];
}

export async function history(signal?: AbortSignal): Promise<SweepRow[]> {
  const resp = await client.history({}, { signal });
  return resp.records.map((r) => {
    const details: SweepDetail[] = r.details.map((d) => ({
      kind: d.kind, target: d.target, rule: d.rule, reason: d.reason, executed: d.executed, error: d.error || undefined,
    }));
    return {
      at: ts(r.at),
      mode: r.quietTicks > 0 ? "quiet" : r.dryRun ? "dry-run" : "executed",
      source: r.source,
      actions: details.length,
      executed: details.filter((d) => d.executed).length,
      failed: details.filter((d) => d.error).length,
      kept: r.kept,
      problems: r.problems,
      quietTicks: r.quietTicks,
      details,
    };
  });
}

export interface Me { name?: string; email?: string; role?: string }

export async function fetchMe(signal?: AbortSignal): Promise<Me & { admin: boolean }> {
  const resp = await client.getMe({}, { signal });
  return {
    // Never the raw subject: for a human that is the IdP's numeric user id.
    // With no name the badge falls back to the email, which reads right.
    name: resp.name || undefined,
    email: resp.email || undefined,
    role: resp.admin ? "admin" : undefined,
    admin: resp.admin,
  };
}

export async function fetchVersion(signal?: AbortSignal): Promise<string> {
  const resp = await client.getVersion({}, { signal });
  return resp.version;
}
