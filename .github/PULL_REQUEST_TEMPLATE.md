## What does this PR change?

<!-- A short description of the change and the motivation behind it. -->

## Related issue

<!-- e.g. Closes #123, refs #456, or "none". -->

## How was this tested?

<!-- Commands you ran and/or the scenario you exercised. Paste relevant output. -->

```
go build ./... && go vet ./... && golangci-lint run && go test -race ./...
```

## Checklist

- [ ] I ran `go build ./... && go vet ./... && golangci-lint run && go test -race ./...` and it passes.
- [ ] I added or updated tests for the behaviour I changed.
- [ ] I updated the documentation (`docs/`, `README.md`) where needed.
- [ ] Every commit is signed off (`git commit -s`), as required by the DCO (`CONTRIBUTING.md`).
- [ ] I did not include secrets, credentials or private keys in code, logs or screenshots.
