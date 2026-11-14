// Package git runs git for the service.
package git

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Git runs git in one directory.
type Git struct {
	Dir string
	// Token, when set, is sent to the remote as GitHub expects an App's
	// installation token. It goes in the environment, not the arguments, so
	// it is not in the process list, and never in a config file the bot's
	// worktree can read.
	Token string
	// Author signs commits.
	Name, Email string
	// Env is added to git's environment, such as GIT_INDEX_FILE.
	Env []string
}

// Run runs git and returns its output, trimmed.
func (g Git) Run(args ...string) (string, error) {
	out, err := g.run(args...)
	return strings.TrimSpace(out), err
}

func (g Git) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Dir
	env := os.Environ()
	// Nothing from the user's git config decides how the service's git
	// behaves: a credential helper there would push with the owner's own
	// token, and commit signing would fail without a key.
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_EDITOR=true")
	if g.Name != "" {
		env = append(env, "GIT_AUTHOR_NAME="+g.Name, "GIT_AUTHOR_EMAIL="+g.Email,
			"GIT_COMMITTER_NAME="+g.Name, "GIT_COMMITTER_EMAIL="+g.Email)
	}
	if g.Token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.Token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic)
	}
	cmd.Env = append(env, g.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), &Error{Args: args, Msg: msg, Err: err}
	}
	return stdout.String(), nil
}

// Error is a git command that failed.
type Error struct {
	Args []string
	Msg  string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), e.Msg) }

// ExitCode is git's exit code, or -1 when git did not run.
func (e *Error) ExitCode() int {
	if x, ok := e.Err.(*exec.ExitError); ok {
		return x.ExitCode()
	}
	return -1
}

// Lines splits output into its non-empty lines.
func Lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
