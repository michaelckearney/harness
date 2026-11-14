package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Codex runs each turn as `codex exec`, resuming the thread's session. It
// signs in with ChatGPT, from an auth.json in its home, which is a folder
// of the service's, so the owner's configuration is not used. ChatGPT's
// sign-in refreshes itself, which the proxy cannot do for it, so the bots'
// user can read it: a bot's change could read it, too.
//
// Codex has no way to limit its shell to one command, and its sandbox would
// keep a framework's tests from the caches and browsers they need, so it
// runs without one: its instructions steer it, and the bots' user is all
// that keeps it from the machine. Run it only where there is one.
type Codex struct {
	Command string
	Dir     string // CODEX_HOME
	// AuthFile is the auth.json that `codex login` wrote. It is copied into
	// Dir when it changes; between changes, Codex refreshes its own copy.
	AuthFile string
	Model    string // optional
	Policy   Policy
	Launcher Launcher
}

func (c *Codex) Name() string { return "openai-codex" }

func (c *Codex) Stop() {}

func (c *Codex) Run(ctx context.Context, t Turn) (string, error) {
	if err := os.MkdirAll(c.Dir, 0o770); err != nil {
		return t.Session, err
	}
	if err := c.signIn(); err != nil {
		return t.Session, err
	}
	opts := []string{
		"--json", "--skip-git-repo-check", "--ignore-user-config",
		"-c", `sandbox_mode="danger-full-access"`,
		"-c", `approval_policy="never"`,
		"-c", `forced_login_method="chatgpt"`,
		"-c", `cli_auth_credentials_store="file"`,
	}
	if c.Model != "" {
		opts = append(opts, "-m", c.Model)
	}
	// Codex reads the AGENTS.md files itself, but has no system prompt to
	// add to, so the harness's instructions lead every prompt.
	prompt := "<harness-instructions>\n" + t.System + "\n</harness-instructions>\n\n" + t.Prompt
	var argv []string
	if t.Session == "" {
		argv = append([]string{c.Command, "exec"}, opts...)
		argv = append(argv, "--", prompt)
	} else {
		argv = append([]string{c.Command, "exec", "resume"}, opts...)
		argv = append(argv, t.Session, prompt)
	}
	extra := []string{"CODEX_HOME=" + c.Dir}
	session := t.Session
	var failed string
	err := runLines(ctx, c.Launcher, t.Dir, argv, extra, func(line []byte) {
		id, msg := codexEvent(line, t.Say)
		if id != "" {
			session = id
		}
		if msg != "" {
			failed = msg
		}
	})
	if err == nil && failed != "" {
		err = fmt.Errorf("codex: %s", failed)
	}
	return session, err
}

// signIn puts AuthFile in Codex's home when it is not there yet or has
// changed since it was last put there. A copy Codex has refreshed is kept
// until then: a new sign-in is a new AuthFile.
func (c *Codex) signIn() error {
	data, err := os.ReadFile(c.AuthFile)
	if err != nil {
		return fmt.Errorf("codex: the ChatGPT sign-in: %w", err)
	}
	sum := sha256.Sum256(data)
	seed := hex.EncodeToString(sum[:]) + "\n"
	auth, mark := filepath.Join(c.Dir, "auth.json"), filepath.Join(c.Dir, "auth.json.seed")
	if old, err := os.ReadFile(mark); err == nil && string(old) == seed {
		if _, err := os.Stat(auth); err == nil {
			return nil
		}
	}
	// The bots' user runs Codex, which reads the file and refreshes it.
	if err := writeFile(auth, data, 0o660); err != nil {
		return err
	}
	return writeFile(mark, []byte(seed), 0o640)
}

// writeFile replaces path whole, whoever owns the file there now.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// codexEvent reports one line of `codex exec --json`, and returns the
// session it starts and the error it carries.
func codexEvent(line []byte, say func(kind, text string)) (session, failed string) {
	var e struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
		Error    struct {
			Message string `json:"message"`
		} `json:"error"`
		Item struct {
			Type             string `json:"type"`
			Text             string `json:"text"`
			Command          string `json:"command"`
			AggregatedOutput string `json:"aggregated_output"`
			ExitCode         *int   `json:"exit_code"`
			Status           string `json:"status"`
			Changes          []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &e) != nil {
		return "", ""
	}
	switch e.Type {
	case "thread.started":
		return e.ThreadID, ""
	case "item.started":
		if e.Item.Type == "command_execution" {
			say("tool", "bash: "+compact(e.Item.Command, 200))
		}
	case "item.completed":
		it := e.Item
		switch it.Type {
		case "agent_message":
			say("text", strings.TrimSpace(it.Text))
		case "reasoning":
			say("reasoning", strings.TrimSpace(it.Text))
		case "command_execution":
			kind := "result"
			if it.ExitCode != nil && *it.ExitCode != 0 {
				kind = "error"
			}
			say(kind, "bash: "+compact(it.Command, 200)+"\n"+compact(it.AggregatedOutput, 2000))
		case "file_change":
			var files []string
			for _, ch := range it.Changes {
				files = append(files, ch.Kind+" "+ch.Path)
			}
			say("tool", "edit: "+strings.Join(files, ", "))
		}
	case "turn.failed":
		say("error", e.Error.Message)
		return "", e.Error.Message
	case "error":
		say("error", e.Message)
	}
	return "", ""
}
