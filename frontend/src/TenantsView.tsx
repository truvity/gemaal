import { useMemo, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import MenuItem from "@mui/material/MenuItem";
import Paper from "@mui/material/Paper";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TableSortLabel from "@mui/material/TableSortLabel";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { checkout, decommission, extend, listTenants, type Tenant } from "./api";
import { useAsync } from "./hooks";

type SortKey = "namespace" | "release" | "tier" | "age";

// TenantsView is the checked-out-cluster ledger: every installation the
// housekeeping loop sees, its TTL standing, and the hold/teardown actions
// (checkout, extend, decommission) that were form posts on the SSR panel.
export function TenantsView({ admin }: { admin: boolean }) {
  const [reload, setReload] = useState(0);
  const { data, error } = useAsync((s) => listTenants(s), [reload]);
  const [q, setQ] = useState("");
  const [sortBy, setSortBy] = useState<SortKey>("namespace");
  const [asc, setAsc] = useState(true);
  const [actErr, setActErr] = useState("");
  const [note, setNote] = useState("");
  const [holdHours, setHoldHours] = useState(4);
  const [busy, setBusy] = useState("");
  const refresh = () => setReload((r) => r + 1);

  const act = async (key: string, fn: () => Promise<string>) => {
    setActErr(""); setNote(""); setBusy(key);
    try { setNote(await fn()); refresh(); } catch (e) { setActErr(e instanceof Error ? e.message : String(e)); } finally { setBusy(""); }
  };

  const rows = useMemo(() => {
    const dir = asc ? 1 : -1;
    const key = (t: Tenant) => (sortBy === "age" ? t.age : t[sortBy]);
    return (data?.tenants ?? [])
      .filter((t) => !q || `${t.namespace} ${t.release} ${t.tier} ${t.executionId}`.toLowerCase().includes(q.toLowerCase()))
      .sort((a, b) => dir * key(a).localeCompare(key(b)));
  }, [data, q, sortBy, asc]);

  if (error) return <Alert severity="error">{error}</Alert>;
  if (data === null) return <Typography color="text.secondary">Loading…</Typography>;

  const sortToggle = (k: SortKey) => { if (sortBy === k) setAsc((a) => !a); else { setSortBy(k); setAsc(true); } };
  const head = (k: SortKey, label: string) => (
    <TableCell><TableSortLabel active={sortBy === k} direction={asc ? "asc" : "desc"} onClick={() => sortToggle(k)}>{label}</TableSortLabel></TableCell>
  );

  return (
    <>
      <Box sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap", mb: 2 }}>
        <TextField placeholder="namespace, release, tier…" value={q} onChange={(e) => setQ(e.target.value)} sx={{ width: 240 }} />
        <TextField select label="Hold" value={holdHours} onChange={(e) => setHoldHours(Number(e.target.value))} sx={{ width: 110 }}>
          {[1, 4, 8, 24, 72].map((h) => <MenuItem key={h} value={h}>{h}h</MenuItem>)}
        </TextField>
        <Typography variant="body2" color="text.secondary">applies to Checkout / Extend</Typography>
      </Box>
      {(data.unreadable.length > 0) && (
        <Alert severity="warning" sx={{ mb: 2 }}>
          Partial answer — unreadable: {data.unreadable.map((u) => `${u.namespace} (${u.reason})`).join("; ")}
        </Alert>
      )}
      {actErr && <Alert severity="error" sx={{ mb: 2 }}>{actErr}</Alert>}
      {note && <Alert severity="success" sx={{ mb: 2 }} onClose={() => setNote("")}>{note}</Alert>}
      <TableContainer component={Paper} variant="outlined" sx={{ maxHeight: "calc(100vh - 240px)" }}>
        <Table stickyHeader size="small">
          <TableHead>
            <TableRow>
              {head("namespace", "Namespace")}
              {head("release", "Release")}
              {head("tier", "Tier")}
              {head("age", "Age")}
              <TableCell>TTL</TableCell>
              <TableCell>Hold until</TableCell>
              <TableCell>Pending</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((t) => {
              const key = `${t.namespace}/${t.release}`;
              return (
                <TableRow key={key} hover>
                  <TableCell>{t.namespace}</TableCell>
                  <TableCell>
                    <Typography variant="body2">{t.release} <Chip label={t.backend} variant="outlined" /></Typography>
                    {t.executionId && <Typography variant="caption" color="text.secondary">{t.executionId}</Typography>}
                  </TableCell>
                  <TableCell><Chip label={t.tier || "—"} variant="outlined" /></TableCell>
                  <TableCell>{t.age}</TableCell>
                  <TableCell>{t.ttl}</TableCell>
                  <TableCell>
                    {t.keepUntil
                      ? <Chip label={t.keepUntil} color={t.expired ? "warning" : "success"} variant="outlined" />
                      : <Typography variant="body2" color="text.secondary">—</Typography>}
                  </TableCell>
                  <TableCell>
                    <Chip label={t.pending} color={t.pending === "none" ? "default" : "warning"} variant={t.pending === "none" ? "outlined" : "filled"} />
                  </TableCell>
                  <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
                    <Button disabled={busy !== ""} onClick={() => act(key, async () => { await checkout(t.namespace, t.release, holdHours); return `${key} held for ${holdHours}h`; })}>Checkout</Button>
                    <Button disabled={busy !== ""} onClick={() => act(key, async () => { await extend(t.namespace, t.release, holdHours); return `${key} extended by ${holdHours}h`; })}>Extend</Button>
                    {admin && (
                      <Button color="error" disabled={busy !== ""} onClick={() => {
                        if (!window.confirm(`Decommission ${key}? The server's dry-run mode still applies.`)) return;
                        act(key, async () => {
                          const out = await decommission(t.namespace, t.release, false);
                          return `${key}: ${out.results.length} action(s), ${out.dryRun ? "dry-run" : "executed"}`;
                        });
                      }}>Decommission</Button>
                    )}
                  </TableCell>
                </TableRow>
              );
            })}
            {rows.length === 0 && <TableRow><TableCell colSpan={8}><Typography color="text.secondary">No tenants match.</Typography></TableCell></TableRow>}
          </TableBody>
        </Table>
      </TableContainer>
    </>
  );
}
