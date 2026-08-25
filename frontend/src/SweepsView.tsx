import { useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import IconButton from "@mui/material/IconButton";
import Paper from "@mui/material/Paper";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Typography from "@mui/material/Typography";
import KeyboardArrowDownIcon from "@mui/icons-material/KeyboardArrowDown";
import KeyboardArrowRightIcon from "@mui/icons-material/KeyboardArrowRight";
import { history, type SweepRow } from "./api";
import { useAsync } from "./hooks";

function modeChip(row: SweepRow) {
  switch (row.mode) {
    case "executed": return <Chip label="executed" color="success" />;
    case "dry-run": return <Chip label="dry-run" color="warning" variant="outlined" />;
    default: return <Chip label={`quiet ×${row.quietTicks}`} variant="outlined" />;
  }
}

// SweepsView is the audit trail: one row per sweep execution (quiet loop
// ticks collapsed), expandable to the per-action outcomes.
export function SweepsView() {
  const { data, error } = useAsync((s) => history(s));
  const [open, setOpen] = useState<Record<string, boolean>>({});
  if (error) return <Alert severity="error">{error}</Alert>;
  if (data === null) return <Typography color="text.secondary">Loading…</Typography>;
  return (
    <TableContainer component={Paper} variant="outlined" sx={{ maxHeight: "calc(100vh - 200px)" }}>
      <Table stickyHeader size="small">
        <TableHead>
          <TableRow>
            <TableCell sx={{ width: 36 }} /><TableCell>When</TableCell><TableCell>Mode</TableCell><TableCell>Source</TableCell>
            <TableCell>Actions</TableCell><TableCell>Executed</TableCell><TableCell>Failed</TableCell><TableCell>Kept</TableCell><TableCell>Problems</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {data.map((r, i) => {
            const k = `${r.at}-${i}`;
            const expandable = r.details.length > 0;
            return [
              <TableRow key={k} hover>
                <TableCell>
                  {expandable && (
                    <IconButton size="small" onClick={() => setOpen((o) => ({ ...o, [k]: !o[k] }))}>
                      {open[k] ? <KeyboardArrowDownIcon fontSize="small" /> : <KeyboardArrowRightIcon fontSize="small" />}
                    </IconButton>
                  )}
                </TableCell>
                <TableCell><Typography variant="body2" color="text.secondary">{r.at}</Typography></TableCell>
                <TableCell>{modeChip(r)}</TableCell>
                <TableCell>{r.source || "—"}</TableCell>
                <TableCell>{r.actions}</TableCell>
                <TableCell>{r.executed}</TableCell>
                <TableCell>{r.failed ? <Chip label={r.failed} color="error" /> : 0}</TableCell>
                <TableCell>{r.kept}</TableCell>
                <TableCell>{r.problems ? <Chip label={r.problems} color="warning" variant="outlined" /> : 0}</TableCell>
              </TableRow>,
              expandable && (
                <TableRow key={`${k}-d`}>
                  <TableCell colSpan={9} sx={{ py: 0, border: open[k] ? undefined : 0 }}>
                    <Collapse in={!!open[k]} unmountOnExit>
                      <Box sx={{ display: "flex", flexDirection: "column", gap: 0.5, py: 1 }}>
                        {r.details.map((d, j) => (
                          <Box key={j} sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap" }}>
                            <Chip label={d.kind} variant="outlined" />
                            <Typography variant="body2" sx={{ fontFamily: "monospace" }}>{d.target}</Typography>
                            <Typography variant="body2" color="text.secondary">{d.rule} · {d.reason}</Typography>
                            {d.executed ? <Chip label="executed" color="success" variant="outlined" /> : <Chip label="planned" variant="outlined" />}
                            {d.error && <Chip label={d.error} color="error" variant="outlined" />}
                          </Box>
                        ))}
                      </Box>
                    </Collapse>
                  </TableCell>
                </TableRow>
              ),
            ];
          })}
          {data.length === 0 && <TableRow><TableCell colSpan={9}><Typography color="text.secondary">No sweeps recorded yet.</Typography></TableCell></TableRow>}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
