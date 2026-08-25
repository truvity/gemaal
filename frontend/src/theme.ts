import { createTheme } from "@mui/material/styles";

// The console's Material theme — same shape as the github-roster reference
// console (gateway-auth/docs/console-stack.md), teal swapped for the pump's
// blue.
export const theme = createTheme({
  palette: {
    primary: { main: "#1565c0" },
    background: { default: "#fafafa" },
  },
  typography: {
    fontSize: 13.5,
    h1: { fontSize: "1.6rem", fontWeight: 600 },
    h2: { fontSize: "1.2rem", fontWeight: 600 },
  },
  components: {
    MuiTableCell: { styleOverrides: { root: { paddingTop: 8, paddingBottom: 8 } } },
    MuiChip: { defaultProps: { size: "small" } },
    MuiButton: { defaultProps: { size: "small" } },
    MuiTextField: { defaultProps: { size: "small" } },
  },
});
