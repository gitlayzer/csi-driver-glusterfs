# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Repository scaffolding: `Makefile` (`build`, `test`, `lint`, `vet`, `fmt`, `image`,
  `clean`, `help`), multi-stage `Dockerfile` (Go 1.27 builder, Debian trixie runtime with
  `glusterfs-client`, amd64/arm64), `.golangci.yml` (golangci-lint v2 format),
  `.gitignore` and `.dockerignore`.
- `cmd/glusterfs-csi` entry point with `--mode`, `--endpoint`, `--nodeid`,
  `--drivername`, `--config` and `--version` flags, plus `pkg/version` build information.
- Continuous integration (`.github/workflows/ci.yml`): golangci-lint, `go test -race`
  with coverage total in the log, `gofmt` / `go mod tidy` verification, `make build` with
  a `--version` smoke test, and a DCO sign-off check on pull requests.
- Multi-arch image workflow (`.github/workflows/image.yml`): build-only for pull
  requests, publish to `ghcr.io/gitlayzer/glusterfs-csi` from `main` (`main`,
  `sha-<short>`) and `v*` tags (`{{version}}`, `{{major}}.{{minor}}`), with the GitHub
  Actions build cache.
- Dependabot configuration for the `gomod`, `github-actions` and `docker` ecosystems
  (weekly, grouped updates for `k8s.io/*` and `google.golang.org/*`).
- Community files: issue forms (bug report, feature request) with blank issues disabled,
  pull request template, `CODEOWNERS`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`
  (Contributor Covenant 2.1), `SECURITY.md`, `.editorconfig` and README badges.

### Changed

- README: added CI, image and license badges and a link to `CONTRIBUTING.md`.
