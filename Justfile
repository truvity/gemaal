# Development commands for gemaal

# Disable go.work (a parent workspace interferes with standalone module builds)
export GOWORK := "off"

# Format all Go files
fmt:
    golangci-lint fmt ./...

# Build (compile check)
build: fmt
    go build ./...

# Run unit tests
test:
    go test ./... -coverprofile=coverage.out

# Run linters. `config verify` first: `run` accepts unknown top-level keys
# silently, so a settings block in the wrong place is otherwise invisible.
lint:
    golangci-lint config verify
    golangci-lint run ./...

# Run Go vulnerability check
vuln:
    govulncheck ./...

# Scan tracked files for committed particulars (account ids, ARNs, ECR
# hosts, internal hostnames, secret paths, tokens) — see the script.
leak-canary:
    ./hack/leak-canary.sh

# Regenerate gen/ from proto/ (buf + protoc-gen-go + protoc-gen-connect-go,
# all from devbox). Generated code is COMMITTED so the module is
# `go get`-able without buf installed.
generate:
    buf lint
    buf generate

# Run go mod tidy
tidy:
    go mod tidy

# Clean build artifacts
clean:
    rm -rf dist/ coverage.out

# Run all checks (build + test + lint + vuln)
# Render the chart with the shipped values plus a fully-featured set, and
# prove each matches its golden under tests/golden/gemaal/ (component
# contract C3 — a reviewer sees what a change did to the output, not just
# that it still rendered). Also proves every fixture under
# tests/invalid/gemaal/ is refused by the schema, not just one inline --set.
chart-lint:
    #!/usr/bin/env bash
    set -euo pipefail
    helm lint charts/gemaal
    diff -u tests/golden/gemaal/default.yaml <(helm template gemaal charts/gemaal)
    diff -u tests/golden/gemaal/full.yaml <(helm template gemaal charts/gemaal \
        --set confirm=true \
        --set rbac.sweep.enabled=true \
        --set exposure.enabled=true \
        --set exposure.hostname=gemaal.example.com)
    for fixture in tests/invalid/gemaal/*; do
        if helm template gemaal charts/gemaal --values "$fixture" >/dev/null 2>&1; then
            echo "ERROR: gemaal accepted $fixture — values.schema.json not enforced" >&2
            exit 1
        fi
    done

# Regenerate the golden renders under tests/golden/gemaal/ — review the diff.
golden:
    helm template gemaal charts/gemaal >tests/golden/gemaal/default.yaml
    helm template gemaal charts/gemaal \
        --set confirm=true \
        --set rbac.sweep.enabled=true \
        --set exposure.enabled=true \
        --set exposure.hostname=gemaal.example.com >tests/golden/gemaal/full.yaml

check: build test lint chart-lint vuln leak-canary

# Build a snapshot release locally (no push, no tag)
snapshot:
    goreleaser release --snapshot --clean

# Run the service locally against the example configuration
run:
    go run ./cmd/server --config config.example.yaml
