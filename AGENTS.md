# Repository Guidelines

## Project Structure & Module Organization

This repository is the Go module `github.com/jeessy2/ddns-go/v6`. `main.go` handles startup, service management, and embedded assets. `dns/` contains provider implementations and update orchestration; `config/` manages settings, domains, network interfaces, and webhooks. `web/` holds HTTP handlers and HTML templates, while `static/` contains JavaScript and CSS. Shared helpers live in `util/`, including update and semantic-version packages. Tests sit beside source files as `*_test.go`. Release configuration lives in `.goreleaser.yml`, `Dockerfile`, and `.github/workflows/`.

## Build, Test, and Development Commands

Use the Go version declared in `go.mod`.

- `make build`: build the `ddns-go` binary with version metadata and CGO disabled.
- `make test` or `go test ./...`: run all package tests.
- `go test ./dns -run TestDesecAddUpdateDomainRecords`: run a focused provider test.
- `CGO_ENABLED=1 go test -race ./...`: check for races on a supported platform with a C compiler. The existing `make test-race` target inherits `CGO_ENABLED=0`, so use this direct command.
- `go run . -l 127.0.0.1:9876 -c /tmp/ddns-go-dev.yaml`: start locally with a separate configuration; open `http://127.0.0.1:9876`.

CI runs builds and tests on amd64 and cross-builds arm64 and riscv64.

## Coding Style & Naming Conventions

Format changed Go files with `gofmt`. Follow `.editorconfig`: UTF-8, LF endings, final newlines, no trailing whitespace, tabs for Go, and two spaces for JSON. Use idiomatic Go exported names and short lowercase package names. Match nearby provider filenames and implement the `DNS` interface in `dns/index.go`. No dedicated lint configuration is present.

## Testing Guidelines

Use Go's standard `testing` package, `TestXxx` functions, and table-driven subtests where useful. Mock provider HTTP requests rather than requiring live credentials. Cover changed behavior and regression cases, including unchanged records and API errors. No minimum coverage threshold is configured; run `go test ./...` before submitting.

## Commit & Pull Request Guidelines

Follow recent commit prefixes: `fix:`, `feat(dns):`, and `build(deps):`. Keep subjects specific; English and Chinese both appear in history. Follow the PR template's “What does this PR do?”, “Motivation”, and “Additional Notes” sections. Include relevant issues, validation commands and results, and screenshots for UI changes.

## Security & Configuration

Keep DNS tokens, passwords, and personal YAML configuration out of commits, fixtures, and logs. Use dummy credentials in tests and a separate development configuration.
