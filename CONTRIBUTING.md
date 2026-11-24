# Contributing to OpenChef

Thanks for your interest. OpenChef is in its foundation phase; the design lives in
[`docs/architecture.md`](docs/architecture.md). Please read it before proposing changes
that touch behaviour.

## Development setup

- Go (the version in `go.mod`'s `toolchain` line; the `go` command downloads it if needed)
- [golangci-lint](https://golangci-lint.run/) v2
- Optional: [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck)

```sh
go build ./...
go test -race ./...
golangci-lint run ./...
govulncheck ./...
```

Run the binary from source:

```sh
go run ./cmd/openchef version
```

## Pull requests

- Keep PRs small and focused; one concern per PR.
- `main` must stay green: CI runs lint, vulnerability checks and tests on macOS, Linux
  and Windows.
- Changes that contradict `docs/architecture.md` must amend it in the same PR.
- New dependencies need a short justification in the PR description.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `test:`, `chore:`, `refactor:`).

## Code standards

- Errors are wrapped with context (`fmt.Errorf("doing x: %w", err)`); callers that branch
  on an error get a typed or sentinel error. Never ignore an error.
- No panics outside `main` except for programmer errors that cannot happen at runtime.
- Platform-specific code lives in `_unix.go` / `_windows.go` (or `_darwin.go`,
  `_linux.go`) files with build constraints, never in runtime `GOOS` switches when
  avoidable.
- Tests are table-driven where it helps, run with `-race`, and must not depend on network
  access or installed agent CLIs. Live tests against real CLIs are gated behind
  `OPENCHEF_LIVE=1`.

## License

By contributing you agree that your contributions are licensed under the Apache License
2.0.
