# Contributing to csi-driver-glusterfs

Thanks for your interest. This document covers the development setup, commit and
sign-off rules, and the pull request flow.

Please read [AGENTS.md](AGENTS.md) and the documentation in [`docs/`](docs/) before
changing code — they describe the architecture, the hard rules of the driver and the
task breakdown. Contributors are expected to follow the same rules as coding agents.

## Development environment

Requirements:

- **Go** — CI builds and tests with the latest supported release (currently **1.27**).
  `go.mod` keeps a lower `go` directive so downstream consumers can build with an
  older toolchain, but please develop and test with a supported one.
- **make** — the build entry point.
- **golangci-lint v2.13.2** — the same version CI runs (`.golangci.yml` uses the v2 format).
- Optional: Docker with buildx for `make image`, a GlusterFS cluster for manual testing.

Common targets:

```sh
make build   # compile bin/glusterfs-csi with version information
make test    # go test -race ./...
make lint    # golangci-lint run
make vet     # go vet ./...
make fmt     # gofmt -w on all tracked Go files
make image   # multi-arch (linux/amd64,linux/arm64) image build, no push
make clean   # remove build artifacts
```

Format the code with `gofmt` and make sure the full check suite passes before opening a
pull request:

```sh
go build ./... && go vet ./... && golangci-lint run && go test -race ./...
```

## Commit message convention

This project uses [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<optional scope>): <description>

<optional body>

Signed-off-by: Your Name <you@example.com>
```

Allowed types: `feat`, `fix`, `docs`, `build`, `ci`, `chore`, `refactor`, `perf`,
`test`, `style`, `revert`. Scopes are free-form, e.g. `driver`, `gluster`, `mounter`,
`deploy`, `docs`, `ci`. Use `!` or a `BREAKING CHANGE:` footer to mark breaking changes.

Examples:

```
feat(controller): implement CreateVolume for subdir mode
fix(mounter): recover from stale FUSE mounts on node startup
docs: describe the subdir provisioning mode
```

## Developer Certificate of Origin (DCO)

Every contribution must be signed off under the
[Developer Certificate of Origin 1.1](https://developercertificate.org/). The sign-off
is a trailer in the commit message that certifies you have the right to submit the code
under the project's license (Apache-2.0):

```
Signed-off-by: Your Name <you@example.com>
```

The easiest way is to let Git add the trailer for you:

```sh
git commit -s -m "fix(gluster): parse quota XML output"
```

If you forgot:

```sh
# last commit
git commit --amend -s --no-edit

# a branch with several commits
git rebase --signoff main
```

CI runs a `dco` job on every pull request that fails when a non-merge commit in the
range lacks a `Signed-off-by:` trailer, so please sign off before pushing.

## Pull request process

1. Open an issue first for non-trivial changes so the approach can be agreed on.
2. Fork the repository and create a topic branch off `main`
   (e.g. `feat/subdir-quota`). Do not push to `main` directly.
3. Keep changes focused: one logical change per pull request, with tests for every new
   function that contains logic.
4. Follow the coding rules in [AGENTS.md](AGENTS.md): argv slices instead of shell
   strings, `gluster --mode=script --xml` with XML parsing, validation of every value
   that comes from a CSI request / StorageClass parameter / PV attribute, no secrets in
   logs, no panics in RPC paths, idempotent RPCs, correct CSI error codes.
5. Fill in the pull request template: what changed, the related issue, how you tested it,
   and the checklist. All CI jobs (`lint`, `test`, `verify`, `build`, `dco`) and the
   image build must pass.
6. Address review comments by pushing additional signed-off commits (or amend and force
   push to your own branch). Merging is done by a maintainer with a linear history.

## Where to look

- [AGENTS.md](AGENTS.md) — hard rules for every change, CSI error code table.
- [`docs/architecture.md`](docs/architecture.md) — architecture, provisioning modes, repo layout.
- [`docs/decisions.md`](docs/decisions.md) — accepted design decisions.
- [`docs/roadmap.md`](docs/roadmap.md) and [`docs/tasks/`](docs/tasks/) — milestones and tasks.
- [`docs/usage.md`](docs/usage.md) — intended user-facing usage.
- [SECURITY.md](SECURITY.md) — how to report a vulnerability privately.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).
