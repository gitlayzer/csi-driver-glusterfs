# AGENTS.md — rules for every coding agent working in this repo

This is a Kubernetes CSI driver for GlusterFS. Read `docs/architecture.md` before changing code.
Your task spec lives in `docs/tasks/T-xx.md`. Do exactly that task; do not touch files outside its scope.

## Hard rules
- Go, standard layout described in `docs/architecture.md` §6. Format with gofmt; must pass `golangci-lint run`.
- Never build shell command strings. External commands are executed with an argv slice
  (`exec.CommandContext(ctx, "gluster", args...)`) or, over SSH, with every argument shell-quoted by `pkg/gluster`.
- Every value that comes from a CSI request, StorageClass parameter or PV attribute must be validated
  (`pkg/volumeid` regex `^[A-Za-z0-9._-]{1,128}$`) before it is used in a path or a command. Reject `..`.
- Never log secrets (CSI request `Secrets` maps, SSH keys). Use `protosanitizer.StripSecrets` when logging requests.
- No panics in RPC paths. Respect `ctx` cancellation. No goroutine without a stop path.
- Every RPC must be idempotent. Repeating a successful call must succeed with the same result.
- Do not add a new dependency unless the task spec allows it; say why in your final report.
- Unit tests are required for every new function with logic. Use table-driven tests. Tests must assert behavior.
- Use `k8s.io/mount-utils` for mount operations and its `FakeMounter` in tests.
- Use `gluster --mode=script --xml` and parse XML; never parse human-readable gluster output.

## CSI error codes (must follow the spec)
| Situation | Code |
|---|---|
| Missing/empty required field (volume_id, target_path, capabilities, name) | `InvalidArgument` |
| Unsupported capability (Block access type, unknown access mode) | `InvalidArgument` |
| Volume does not exist (Node*/Validate/Expand) | `NotFound` |
| DeleteVolume on a non-existent volume | **OK** (success) |
| CreateVolume same name, incompatible size/params | `AlreadyExists` |
| Another operation on the same volume is in flight | `Aborted` |
| Not enough capacity | `ResourceExhausted` |
| Capability not implemented | `Unimplemented` |
| Backend (gluster/ssh/mount) failure | `Internal` (include a short cause, no secrets) |

## Before you finish
Run and make sure all pass, then report the output summary:
```
go build ./... && go vet ./... && golangci-lint run && go test -race ./...
```
Final report must list: files changed, design decisions, anything in the spec you could not do.
