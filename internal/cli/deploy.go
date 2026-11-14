package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/michaelckearney/harness/internal/deploy"
	"github.com/michaelckearney/harness/internal/version"
)

const deployUsage = `usage: harness deploy [-dry-run] -server <file> <github-url>

On the server, as the user the service runs as: sets up the bots for the
monorepo at <github-url>, such as https://github.com/owner/monorepo. It
installs this harness binary, writes the service's configuration and keys,
makes the bots' user, and installs and restarts the systemd user unit. A
service that does not come back healthy is put back as it was.

From then on the service follows the harness version the monorepo's
harness.yaml pins on its default branch: it updates itself, and if the new
one cannot stay up, the one before it goes back. Deploy again only when the
server's file or secrets change.

  -server <file>  the server's file: its agents, model, GitHub App, and API
  -dry-run        print the script, secrets and all, instead of running it

The server's file:

  schema: 1
  agents: [opencode, claude-code]
  sessions: 1                 # turns at once
  bot_user: harness-bot       # the user the agents run as, which deploy makes
  model: { base_url: http://127.0.0.1:8000/v1, id: qwen3.6, vision: true }
  github: { app_id: 123456 }  # the GitHub App the bots post as
  api: { port: 7420, allow: [me@example.com] }  # the Slipway app, on the tailnet

The secrets come from the environment:

  HARNESS_GITHUB_APP_KEY   the GitHub App's private key (with github.app_id)
  HARNESS_GITHUB_TOKEN     or a token instead of an App, to try things out
  CLAUDE_CODE_OAUTH_TOKEN  for a server that runs claude-code: claude setup-token
  CODEX_AUTH_JSON          for a server that runs openai-codex: codex login's auth.json
  HARNESS_MODEL_API_KEY    the model endpoint's key, if it has one
  HARNESS_OWNER_GH_TOKEN   the owner's token, for uploading the bots' images
`

// Deploy runs `harness deploy`.
func Deploy(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := deploy.Options{Version: version.Get()}
	server := fs.String("server", "", "")
	fs.BoolVar(&o.DryRun, "dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, deployUsage)
			return nil
		}
		return fail("harness deploy: %v\n\n%s", err, deployUsage)
	}
	if *server == "" || fs.NArg() != 1 {
		return fail("harness deploy: give -server <file> and the monorepo's URL\n\n%s", deployUsage)
	}
	repoName, err := deploy.RepoOf(fs.Arg(0))
	if err != nil {
		return &Error{err.Error()}
	}
	o.Repo = repoName
	s, err := deploy.LoadServer(*server)
	if err != nil {
		return &Error{err.Error()}
	}
	if runtime.GOOS != "linux" && !o.DryRun {
		return fail("the service runs as a systemd unit, and this is %s: run harness deploy on the server", runtime.GOOS)
	}
	if o.Exe, err = os.Executable(); err != nil {
		return err
	}
	owner, _, _ := strings.Cut(repoName, "/")
	if o.Author, err = deploy.Noreply(ctx, owner); err != nil {
		return &Error{err.Error()}
	}
	if err := deploy.Run(ctx, s, o, stdout); err != nil {
		return &Error{err.Error()}
	}
	return nil
}
