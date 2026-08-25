// Package frontend embeds the built web console (Vite/React/MUI — the fleet
// console stack; see gateway-auth/docs/console-stack.md). dist/ is committed
// so `go build` needs no Node toolchain.
package frontend

import "embed"

// Assets is the built single-page app.
//
//go:embed dist
var Assets embed.FS
