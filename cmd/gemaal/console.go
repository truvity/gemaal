package main

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/truvity/gemaal/frontend"
)

// consoleDist is the built SPA (Vite/React/MUI — the fleet console stack;
// gateway-auth/docs/console-stack.md) rooted at its dist/ directory.
var consoleDist, _ = fs.Sub(frontend.Assets, "dist")

// registerConsole serves the web console at the root: GET / is the SPA shell
// and /assets/* its content-hashed build files. Views live in the URL
// fragment (#sweeps), which never reaches the server, so no catch-all is
// needed — the ConnectRPC path and /healthz keep their routes untouched.
// Replaces the SSR panel (pkg/panel).
func registerConsole(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		data, err := fs.ReadFile(consoleDist, "index.html")
		if err != nil {
			http.Error(w, "console is not built", http.StatusNotFound)

			return
		}

		harden(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		name := "assets/" + strings.TrimPrefix(r.URL.Path, "/assets/")

		data, err := fs.ReadFile(consoleDist, name)
		if err != nil {
			http.Error(w, "no such asset", http.StatusNotFound)

			return
		}

		harden(w)

		if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}

		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write(data)
	})
}

// harden bounds what a rendered page can do; the gateway authenticates.
func harden(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'")
}
