package agents

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeCode runs each turn as `claude -p`, resuming the thread's session.
// It reaches Anthropic through the proxy, which holds the Claude
// subscription's token, so Claude Code never holds it, and
// its configuration is a folder of the service's, so neither the owner's
// settings nor a project's .claude/settings.json can widen what it may do.
type ClaudeCode struct {
	Command  string
	Dir      string // its configuration folder: CLAUDE_CONFIG_DIR
	BaseURL  string // the proxy's route to Anthropic
	Key      string // the proxy's token
	Model    string // optional
	Policy   Policy
	Launcher Launcher
}

func (c *ClaudeCode) Name() string { return "claude-code" }

func (c *ClaudeCode) Stop() {}

// tools are what the bot may use without asking: in print mode anything
// else is refused. The shell runs only the harness.
func (c *ClaudeCode) tools() (allowed, disallowed []string) {
	cmd := c.Policy.Command
	allowed = []string{
		"Bash(" + cmd + ")", "Bash(" + cmd + " *)",
		"Read", "Edit", "MultiEdit", "Write", "Glob", "Grep", "LS", "NotebookEdit", "TodoWrite",
		"WebFetch", "WebSearch",
	}
	disallowed = []string{"AskUserQuestion"}
	return allowed, disallowed
}

// settings are the user settings in the service's folder for Claude Code.
// The project's are not read: see Run.
func (c *ClaudeCode) settings() ([]byte, error) {
	allowed, disallowed := c.tools()
	s := map[string]any{
		"permissions": map[string]any{
			"allow": allowed,
			// Deny wins over allow, so the shell is limited by allowing only
			// the harness: print mode refuses what is not allowed.
			"deny":                  disallowed,
			"defaultMode":           "dontAsk",
			"additionalDirectories": c.Policy.ReadDirs,
		},
		"includeCoAuthoredBy": false,
	}
	data, err := json.MarshalIndent(s, "", "  ")
	return append(data, '\n'), err
}

func (c *ClaudeCode) Run(ctx context.Context, t Turn) (string, error) {
	if err := os.MkdirAll(c.Dir, 0o770); err != nil {
		return t.Session, err
	}
	data, err := c.settings()
	if err != nil {
		return t.Session, err
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "settings.json"), data, 0o640); err != nil {
		return t.Session, err
	}
	allowed, disallowed := c.tools()
	argv := []string{c.Command, "-p", "--output-format", "stream-json", "--verbose",
		// Only the service's settings: a project's could allow any command.
		"--setting-sources", "user",
		"--permission-mode", "dontAsk",
		"--append-system-prompt", t.System,
		"--allowedTools", strings.Join(allowed, ","),
		"--disallowedTools", strings.Join(disallowed, ","),
	}
	for _, d := range c.Policy.ReadDirs {
		argv = append(argv, "--add-dir", d)
	}
	if c.Model != "" {
		argv = append(argv, "--model", c.Model)
	}
	session := t.Session
	if session == "" {
		session = newUUID()
		argv = append(argv, "--session-id", session)
	} else {
		argv = append(argv, "--resume", session)
	}
	argv = append(argv, "--", t.Prompt)
	extra := []string{
		"CLAUDE_CONFIG_DIR=" + c.Dir,
		"ANTHROPIC_BASE_URL=" + c.BaseURL,
		"ANTHROPIC_API_KEY=" + c.Key,
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	var failed string
	err = runLines(ctx, c.Launcher, t.Dir, argv, extra, func(line []byte) {
		if msg := c.event(line, t.Say); msg != "" {
			failed = msg
		}
	})
	if err == nil && failed != "" {
		err = fmt.Errorf("claude: %s", failed)
	}
	return session, err
}

// event reports one line of Claude Code's stream, and returns the error a
// result line carries.
func (c *ClaudeCode) event(line []byte, say func(kind, text string)) string {
	var e struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				Thinking  string          `json:"thinking"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
				ToolUseID string          `json:"tool_use_id"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil {
		return ""
	}
	switch e.Type {
	case "assistant":
		for _, p := range e.Message.Content {
			switch p.Type {
			case "text":
				if s := strings.TrimSpace(p.Text); s != "" {
					say("text", s)
				}
			case "thinking":
				if s := strings.TrimSpace(p.Thinking); s != "" {
					say("reasoning", s)
				}
			case "tool_use":
				say("tool", p.Name+": "+compact(string(p.Input), 200))
			}
		}
	case "user":
		for _, p := range e.Message.Content {
			if p.Type != "tool_result" {
				continue
			}
			text := toolText(p.Content)
			if p.IsError {
				say("error", compact(text, 2000))
			} else {
				say("result", compact(text, 2000))
			}
		}
	case "result":
		if e.IsError || (e.Subtype != "" && e.Subtype != "success") {
			msg := e.Result
			if msg == "" {
				msg = e.Subtype
			}
			say("error", msg)
			return msg
		}
	}
	return ""
}

// toolText is a tool result's content, which is a string or a list of
// parts.
func toolText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
