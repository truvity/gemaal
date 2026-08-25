import { useEffect, useState } from "react";
import AppBar from "@mui/material/AppBar";
import Box from "@mui/material/Box";
import Container from "@mui/material/Container";
import Tab from "@mui/material/Tab";
import Tabs from "@mui/material/Tabs";
import Toolbar from "@mui/material/Toolbar";
import Typography from "@mui/material/Typography";
import { UserBadge, signOutUrl } from "@truvity/gateway-auth/react";
import { fetchMe, fetchVersion } from "./api";
import { useAsync } from "./hooks";
import { TenantsView } from "./TenantsView";
import { SweepsView } from "./SweepsView";

type TabKey = "tenants" | "sweeps";
const TABS: TabKey[] = ["tenants", "sweeps"];

// Views live in the URL fragment (#sweeps) so refresh, back/forward and deep
// links work — and the server needs no catch-all route.
function tabFromHash(): TabKey {
  const h = (typeof location !== "undefined" ? location.hash.replace(/^#/, "") : "") as TabKey;
  return TABS.includes(h) ? h : "tenants";
}

function VersionBadge() {
  const { data } = useAsync(fetchVersion);
  if (!data) return null;
  return <Typography variant="caption" color="text.secondary">{data}</Typography>;
}

export function App() {
  const [tab, setTab] = useState<TabKey>(tabFromHash);
  const { data: me } = useAsync(fetchMe);
  useEffect(() => {
    const onHash = () => setTab(tabFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  const labels: Record<TabKey, string> = { tenants: "Tenants", sweeps: "Sweeps" };
  return (
    <>
      <AppBar position="sticky" color="default" elevation={0} sx={{ borderBottom: 1, borderColor: "divider", bgcolor: "background.paper" }}>
        <Toolbar variant="dense" sx={{ gap: 2 }}>
          <Typography component="span" sx={{ fontWeight: 700 }}>gemaal</Typography>
          <Tabs value={tab} sx={{ minHeight: 48 }}>
            {TABS.map((k) => <Tab key={k} value={k} label={labels[k]} component="a" href={`#${k}`} sx={{ minHeight: 48 }} />)}
          </Tabs>
          <Box sx={{ ml: "auto" }}>
            <UserBadge me={me} signOutHref={signOutUrl()} emphasizeRoles={["admin"]} />
          </Box>
        </Toolbar>
      </AppBar>
      <Container maxWidth="lg" sx={{ py: 3 }}>
        <Typography variant="h1" sx={{ mb: 2 }}>{labels[tab]}</Typography>
        {tab === "tenants" && <TenantsView admin={!!me?.admin} />}
        {tab === "sweeps" && <SweepsView />}
        <Box component="footer" sx={{ mt: 6, pt: 2, borderTop: 1, borderColor: "divider", textAlign: "center" }}>
          <VersionBadge />
        </Box>
      </Container>
    </>
  );
}
