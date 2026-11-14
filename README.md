# harness

A framework for monorepos whose GitHub issues are worked by bots. The
harness is what every monorepo shares; what each one does is its own.

- **The harness** (this repository) is versioned and shared. It runs a
  monorepo's verbs on its framework, runs the bots that work its issues,
  sets them up on the servers they run on, and releases its projects.
- **The framework** is a folder in the monorepo, one per monorepo: the code
  and tooling its projects share, for whatever kinds of project they are.
  It belongs to its monorepo and changes with its projects: a breaking
  change and its fixes land in one pull request. Another monorepo starts
  from a copy (`harness init -from`), which is its own from then on.
- **Projects** are folders the framework builds.

```
harness.yaml        the harness it uses, the projects, the bots
framework/          framework.yaml, and whatever the framework is
projects/
  harbor/           a project, in its framework's layout
.github/            the pipelines, which call this repository's
```

## harness.yaml

```yaml
schema: 1
harness: v0.7.0              # the harness the monorepo uses; servers follow it
repo: owner/name
triggers: [owner]            # who can give the bots work
agents:
  default: opencode
  allowed: [opencode, claude-code, openai-codex]
prepare: go mod download     # optional: run in a bot's worktree before each turn
projects:
  harbor: projects/harbor    # each project's folder
```

The framework is always in `framework/`. Nothing in it is about a machine:
the servers, the model, the GitHub App and the keys are each server's own
(see [Servers](#servers)), so a monorepo can be public.

`harness check` checks it, and that the framework and each project are there.

## Verbs

```bash
harness test [-p <name>] [args]    # lint and tests
harness build [-p <project>]       # build a project's release artifacts
harness <verb> [-p <project>]      # one of the framework's own verbs
harness help                       # lists them, with the framework's verbs
```

`test` and `build` are the harness's: they mean the same in every monorepo,
`done` relies on `test`, and releases on `build`. Every other verb is the
framework's own, such as `run` or `record`: the harness passes it on, and
knows nothing more of it.

-p names a project, or `framework` for the framework and every project.
Without it, a verb is for the project whose folder it runs in, or, anywhere
else, the framework and every project. `test` is the only verb with more
than one target: the framework's own code, then each project. It stops at
the first target that fails. The arguments after the verb are the
framework's.

## The contract

The framework is a folder with a `framework.yaml`:

```yaml
command: [go, run, ./cmd/desktop]
verbs:
  test: lint, unit tests, then e2e tests     # required
  build: build the release artifacts         # for releases
  run: start the app for development
  record: { help: drive the app and record it, turn: true }
```

Each verb has a line of help, for `harness help`. A bot may run `test` in
its turn, never `build`, and another verb only with `turn: true`: one that
returns and does nothing that is a person's to do. A verb may not take a
name the harness uses, such as `commit` or `check`.

The harness runs the command in the framework's folder, with the verb and
its arguments after it, and this environment:

| Variable | |
|---|---|
| `HARNESS_ROOT` | the monorepo's root |
| `HARNESS_FRAMEWORK` | the framework's folder |
| `HARNESS_PROJECT` | the project's folder, or empty for the framework's own code (test only) |
| `HARNESS_PROJECT_NAME` | the project's name |
| `HARNESS_CWD` | where the harness was run, for arguments that are paths |
| `HARNESS_VERSION` | for build: the version to build |
| `HARNESS_TURN` | `1` during a bot's turn |
| `HARNESS_RESULT` | a file for the verb's result, as JSON |

The exit code says whether the verb worked. The result says the rest:
`{"full": true}` when test ran everything with no filter, and
`{"artifacts": ["build/app.zip"]}` for what build made, relative to the
project. A framework in Go can use package `contract`, whose `Main` reads
the call and writes the result.

## Bots

`harness serve` on a server works the monorepo's GitHub issues. An issue
opened by a trigger user starts a **thread**: one issue, one branch
(`harness/<number>`), one worktree, one agent session, and later one pull
request. Each turn answers what is new: a trigger user's issue, comment, or
review, CI failing on the bot's last push, or the default branch moving on.
When the default branch moves on only in other projects, without conflicts,
a project's thread is not asked: the harness merges it into the branch
itself, and CI checks the result. The bot replies in the issue, keeps its pull request's title and
description in `.harness/pr.md` in its worktree, and ends every turn with
`harness done`. You merge.

During a turn the bot's shell runs only the harness:

```
harness status              the request, the conversation, your changes, CI
harness diff                your changes against the default branch
harness commit "<what>"     commit everything and push, and publish .harness/pr.md
harness sync                merge the default branch into yours
harness rm <path>...        delete files or folders from your worktree
harness reply -             say something in the conversation
harness done                end the turn
harness test [args]         test what this issue works on
harness record [args]       drive a project's app and record it
```

`.harness/pr.md` is the bot's pull request: its first line is the title, a
conventional commit, and the rest the description. A new worktree gets a
template, and git ignores the file. Every `harness commit` checks it, then
opens the pull request from it or updates the pull request to match it.
What anyone else writes in the pull request's title or description is put
back between turns, and the bot is not told: to the bot, the file is the
pull request.

`done` refuses until the bot has replied (if a person asked something),
committed and pushed everything, including the latest `.harness/pr.md`,
and run a full `harness test` of the issue's scope on what it committed. A
failing run still ends the turn; the pull request stays a draft.

Only the latest run of each check counts: a failure that a later run of the
same check replaced does not start a turn.

### Labels

Labels choose who works an issue and on what. Only a trigger user's labels
count: a label can pick an agent that costs money or sends the code
elsewhere.

| Label | |
|---|---|
| `agent:<name>` | `opencode`, `claude-code`, or `openai-codex`; `agents.default` without one |
| `project:<name>` | the issue is for one project |
| `framework` | the issue is for the framework and every project |

An issue with no scope label is no bot's: nothing works it until a trigger
user labels it. `harness commit` refuses a change outside the scope.

In a turn, `harness test` with no arguments tests what the issue's branch
changed: the projects it changed, or, when it changed anything else, the
framework and every project. A full run of that counts for `done`. Each
pull request's CI tests everything.

### What keeps the bots in their place

- **The bots hold no credentials, except Codex's ChatGPT sign-in.** The
  service holds the GitHub App's key and does the bots' GitHub and git work
  over a Unix socket. opencode and Claude Code reach their models through
  the service's proxy, which swaps the proxy's token for the real one:
  Claude Code never sees the Claude subscription's token. Codex signs in
  with ChatGPT, whose sign-in refreshes itself, so it keeps its `auth.json`
  where the bots' user can read it: a Codex bot, or a bot's change to a
  framework's tests, can read that sign-in.
- **The service decides, not the bot.** The checks (`done`, the commit's
  scope, who counts as a trigger) are in the service's binary, pinned by
  harness.yaml. A framework's code runs from the bot's worktree, so a bot
  can change what `test` does, never whether `done` lets it finish.
- **The agents' rules steer; the user stops.** opencode's and Claude Code's
  shells run only the harness. Codex's rules cannot allow one command and
  refuse the rest, and its sandbox would keep a framework's tests from the
  caches and browsers they need, so the harness runs it without the
  sandbox, steered by its instructions. On a server with `bot_user`, the agents run
  as that user: no sudo, no gh login, no say over the service.
- **harness.yaml is read from the default branch only**, so a change to the
  bots takes effect when a person merges it.
- **Pull requests get no secrets.** The bots push branches to the same
  repository, so anything a pull request's workflow could read, a bot's
  change could read. The secrets are only on the servers.

### Each agent

| Agent | Session | Model | Shell |
|---|---|---|---|
| opencode | `opencode serve`, one session per thread | the server's `model`, through the proxy | the harness only |
| claude-code | `claude -p`, resumed per thread | a Claude subscription, through the proxy | the harness only |
| openai-codex | `codex exec`, resumed per thread | a ChatGPT subscription, signed in itself | anything: the bots' user is its only limit |

Each agent's instructions are the harness's, then what the issue works on,
then the `AGENTS.md` (or `CLAUDE.md`) of the framework and of the project.

## Servers

Each server runs one service per monorepo, with its own configuration,
state, and bots' user, so two monorepos on one server share nothing. Give
each allowed agent to one server, so each issue has one owner.

`harness deploy`, run on the server as the user the service runs as, sets
the service up for the monorepo at a URL, from the server's own file and
secrets. However the server is reached is not the harness's: run it by
hand, or from whatever sets the machine up, such as Ansible.

```yaml
# server.yaml
schema: 1
agents: [opencode, claude-code, openai-codex]
bot_user: harness-bot        # optional: who the agents run as
model: { base_url: http://127.0.0.1:8000/v1, id: qwen3.6, vision: true }
github: { app_id: 123456 }   # the GitHub App the bots post as
api: { port: 7420, allow: [me@example.com] }   # optional: the Slipway app, on the tailnet
```

```bash
HARNESS_GITHUB_APP_KEY="$(cat app.pem)" ... harness deploy -server server.yaml https://github.com/owner/name
```

It installs its own binary, writes the service's configuration and keys,
makes the bots' user, and starts the systemd user unit; a service that does
not come back on the new version is put back as it was. The secrets come
from its environment: `HARNESS_GITHUB_APP_KEY`, and as the server's agents
need them `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`),
`CODEX_AUTH_JSON` (the `~/.codex/auth.json` that `codex login` writes),
`HARNESS_MODEL_API_KEY`, and `HARNESS_OWNER_GH_TOKEN` (for uploading the
bots' images: GitHub's upload endpoint refuses App tokens). The bots'
commits are by `harness`, with the GitHub noreply address of the monorepo's
owner. It needs on the server: Linux with systemd, git, and the agents'
commands; and passwordless sudo for `bot_user`. What else the machine
runs, such as the model server, is not the harness's.

From then on, **the service follows the harness harness.yaml pins** on the
default branch, up or down: it downloads that release, checks it against
`SHA256SUMS` and that it accepts the service's configuration, puts it in
place of its own binary, and, once the running turns finish, exits for
systemd to start the new one. If the new one cannot stay up (five starts
in ten minutes), the previous binary goes back, and the service stays off
that release until harness.yaml pins another or someone deploys. It never
moves to a release before v0.7.0, which could not move again. Deploy again
only when the server's file or secrets change.

The Claude token lasts about a year: make a new one with `claude
setup-token` and deploy with it. Codex refreshes its sign-in itself. The
service copies `CODEX_AUTH_JSON` into Codex's home only when it changes, so
a deploy never replaces a refreshed sign-in with an old one; to sign in
again, deploy with a new one.

```bash
systemctl --user status harness-<repo>
journalctl --user -u harness-<repo> -f
harness queue --config ~/.config/harness/<repo>/host.yaml
```

## Pipelines

Each pipeline is one command, `harness ci <pipeline>`, so a CI service only
checks out the monorepo, downloads the harness release harness.yaml pins
(`harness-linux-amd64`, checked against `SHA256SUMS`), and runs it.
`harness init` writes the two GitHub workflows that do that:

- **pull request**: `harness ci pr origin/<base>`: `harness check`, then
  `harness test -since` the base branch: a change in one project tests that
  project, any other change everything. No secrets. A newer push cancels
  the run for the older one. Nothing checks the title of a pull request a
  person opens, or the commit message a person writes when merging.
- **main**: `harness ci release` (`harness image -push`, then `harness
  release`) and `harness ci test` (everything, to keep the pull requests'
  cache), side by side. Nothing deploys: the servers follow harness.yaml.

Moving to another CI service is rewriting those few lines in its syntax:
nothing else in the pipelines is GitHub's.

The pipelines know nothing of the framework's tools: the harness runs on
the runner, and runs the framework in its image, built from
`framework/ci/Dockerfile` (`harness image -h`). The image is tagged with
`framework/ci`'s git tree, so it is built only when that folder changes:
published from main, and built in a pull request that changes it until
then. The Dockerfile's `VOLUME` folders are its caches, such as Go's and
npm's, so the framework says what to cache, in whatever language it is.
They are kept in the registry beside the image, as `ci-cache-test` and
`ci-cache-release`: main writes them, and pull requests read the test
cache and never write it, so a new branch starts warm and no branch
changes what another starts from. On GHCR, a save deletes the version it
replaced, which the tag left untagged, so the caches do not pile up. With `GH_TOKEN` set, the pipelines log in
to the registry with it: pull requests need `packages: read`, main
`packages: write`.

## Releases

Every change reaches `main` through a squash-merged pull request whose
title is a conventional commit: the harness makes sure of it for the bots'
pull requests. Each project is released on its own,
tagged `<project>/v<version>`, from the titles of the pull requests merged
since its last tag that changed its folder: `fix:` a patch, `feat:` a minor
release (a patch before 1.0), `feat!:` a major one (minor before 1.0).
`harness release -plan` prints what would be released. Nothing writes a
version into the repository.

The harness itself is released the same way, tagged `v<version>`.
