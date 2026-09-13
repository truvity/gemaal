// Package frontend embeds the built web console (Vite/React/MUI — a console
// behind access-roster; see access-roster/docs/connect/console-app.md).
// dist/ is committed so `go build` needs no Node toolchain.
package frontend

import "embed"

// Assets is the built single-page app.
//
//go:embed dist
var Assets embed.FS
