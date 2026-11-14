// Package agents runs the agents that take the bots' turns: opencode,
// Claude Code, and Codex. Each is set up from the same Policy, in its own
// configuration's terms: what the bot may run, where it may read and write,
// and that it may not ask questions. The policy steers the agent; the user
// the agents run as is what keeps them from the machine.
package agents

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
)

// Turn is one prompt in one thread's session.
type Turn struct {
	// Dir is the thread's worktree.
	Dir string
	// Session is the agent's session for the thread, "" before the first
	// turn.
	Session string
	// Title names a new session, such as gh-12.
	Title string
	// Prompt is what the turn says to the bot.
	Prompt string
	// System is the instructions: the harness's, the framework's, and the
	// scope's.
	System string
	// Say reports what the bot does, for the thread's log and the live view.
	Say func(kind, text string)
}

// Agent runs turns.
type Agent interface {
	Name() string
	// Run runs one prompt, starting the session if there is none, and
	// returns the session.
	Run(ctx context.Context, t Turn) (session string, err error)
	// Stop ends whatever the agent keeps running between turns.
	Stop()
}

// Policy is what every agent is told it may do.
type Policy struct {
	// Command is the one command the bot's shell may run: the harness.
	Command string
	// ReadDirs are folders outside the worktree the bot may read, such as
	// the owner's attachments.
	ReadDirs []string
}

// Launcher starts an agent's process with no credentials of its own: as the
// bots' user when there is one, with only the environment it needs.
type Launcher struct {
	// User, if set, is who the agent runs as, through sudo -n.
	User string
	// Env is the environment every agent gets, such as the service's socket
	// and the host's extra variables.
	Env []string
	// Log is where the agents' own output goes.
	Log io.Writer
	// Path is the agents' PATH; the service's when empty.
	Path string
}

// secret are the variables an agent must never inherit from the service.
var secret = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
	"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_SERVER_PASSWORD",
	"CODEX_HOME", "CLAUDE_CONFIG_DIR", "HARNESS_BOT_SOCKET",
}

// Command is how to start argv with extra variables: argv and the
// environment to give exec.
func (l Launcher) Command(argv []string, extra []string) ([]string, []string) {
	path := l.Path
	if path == "" {
		path = os.Getenv("PATH")
	}
	vars := append(append([]string{}, l.Env...), extra...)
	if l.User == "" {
		vars = append(vars, "PATH="+path)
		var env []string
		set := map[string]bool{}
		for _, kv := range vars {
			name, _, _ := strings.Cut(kv, "=")
			set[name] = true
		}
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			if slices.Contains(secret, name) || set[name] {
				continue
			}
			env = append(env, kv)
		}
		return argv, append(env, vars...)
	}
	// sudo sets SHELL to the bots' user's login shell, which is nologin: an
	// agent runs its commands with SHELL, so each would fail.
	if !slices.ContainsFunc(vars, func(kv string) bool { return strings.HasPrefix(kv, "SHELL=") }) {
		vars = append(vars, "SHELL="+botShell())
	}
	// sudo replaces PATH and drops the rest unless told to keep them.
	var names []string
	for _, kv := range vars {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	env := append([]string{"PATH=" + os.Getenv("PATH")}, vars...)
	out := []string{"sudo", "-n", "-u", l.User, "--preserve-env=" + strings.Join(names, ","), "--",
		"/usr/bin/env", "PATH=" + path}
	return append(out, argv...), env
}

// botShell is the shell the bots' commands run in.
func botShell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

func compact(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
