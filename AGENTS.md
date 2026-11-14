# harness

The harness: the shared, versioned part of every monorepo. README.md is the
reference for what it does.

## Layout

- `cmd/harness/`: the command.
- `contract/`: the public package frameworks use: the verbs, the
  environment, the result.
- `internal/repo/`: harness.yaml and scopes. `internal/host/`: a server's
  configuration. `internal/dispatch/`: runs a verb on a framework, in its
  image in the pipelines (`internal/image/`).
- `internal/cli/`: the developer's verbs, check, init, release; and deploy,
  the server's.
- `internal/service/`: `harness serve`, the bots' service, and its tests,
  which drive it end to end with a fake GitHub and a fake opencode;
  `update.go` is how it follows the harness harness.yaml pins.
- `internal/agents/`: opencode, Claude Code, and Codex.
- `internal/proxy/`: the model proxy that holds the keys.
- `internal/deploy/`: setting up a monorepo's service on the server it runs
  on, from the server's file.
- `internal/cli/ci.go`: the pipelines, `harness ci`; `internal/cache/`
  keeps their caches in the registry.
- `.github/workflows/ci.yml`: the harness's own checks and release.

## Rules

- `go vet ./... && go test ./...` before you are done.
- The checks that keep the bots in their place belong in the service, never
  in a framework or anything else a bot's worktree can change.
- Changing the contract, harness.yaml, or the bots' commands changes every
  monorepo: say so in the pull request, with `!` in the title if it breaks
  one.
- Every change reaches `main` through a squash-merged pull request whose
  title is a conventional commit.
