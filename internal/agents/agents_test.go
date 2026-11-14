package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake writes a script that records its arguments and environment and
// prints out as its stream.
func fake(t *testing.T, out string) (string, string) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + rec + ".args\nenv > " + rec + ".env\ncat <<'EOF'\n" + out + "\nEOF\n"
	bin := filepath.Join(dir, "agent")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, rec
}

func read(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClaudeCode(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "the-service-key")
	bin, rec := fake(t, `{"type":"system","subtype":"init","session_id":"x"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","name":"Bash","input":{"command":"harness status"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"Issue #1"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"done"}`)
	c := &ClaudeCode{Command: bin, Dir: filepath.Join(t.TempDir(), "claude"), BaseURL: "http://127.0.0.1:9/anthropic", Key: "proxy-token",
		Policy: Policy{Command: "harness", ReadDirs: []string{"/state/attachments"}}}
	var said []string
	session, err := c.Run(context.Background(), Turn{Dir: t.TempDir(), Prompt: "Do it.", System: "Rules.", Say: func(k, s string) { said = append(said, k+": "+s) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(session) != 36 {
		t.Errorf("session %q", session)
	}
	args, env := read(t, rec+".args"), read(t, rec+".env")
	for _, want := range []string{"--session-id\n" + session, "--setting-sources\nuser", "--permission-mode\ndontAsk", "Bash(harness *)",
		"--append-system-prompt\nRules.", "--add-dir\n/state/attachments", "--\nDo it."} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if !strings.Contains(env, "ANTHROPIC_API_KEY=proxy-token") || strings.Contains(env, "the-service-key") ||
		!strings.Contains(env, "ANTHROPIC_BASE_URL=http://127.0.0.1:9/anthropic") {
		t.Errorf("env:\n%s", env)
	}
	if got := strings.Join(said, "|"); got != "text: Looking.|tool: Bash: {\"command\":\"harness status\"}|result: Issue #1" {
		t.Errorf("said %s", got)
	}
	if settings := read(t, filepath.Join(c.Dir, "settings.json")); strings.Contains(settings, `"Bash(*)"`) || !strings.Contains(settings, `"Bash(harness *)"`) {
		t.Errorf("settings:\n%s", settings)
	}

	// The next turn resumes it.
	if _, err := c.Run(context.Background(), Turn{Dir: t.TempDir(), Session: session, Prompt: "More.", Say: func(string, string) {}}); err != nil {
		t.Fatal(err)
	}
	if args := read(t, rec+".args"); !strings.Contains(args, "--resume\n"+session) {
		t.Errorf("not resumed:\n%s", args)
	}

	// An error result fails the turn.
	bin, _ = fake(t, `{"type":"result","subtype":"error_max_turns","is_error":true}`)
	c.Command = bin
	if _, err := c.Run(context.Background(), Turn{Dir: t.TempDir(), Say: func(string, string) {}}); err == nil || !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("an error result: %v", err)
	}
}

func TestCodex(t *testing.T) {
	bin, rec := fake(t, `{"type":"thread.started","thread_id":"th-1"}
{"type":"item.started","item":{"type":"command_execution","command":"harness status"}}
{"type":"item.completed","item":{"type":"command_execution","command":"harness status","aggregated_output":"Issue #1","exit_code":0}}
{"type":"item.completed","item":{"type":"agent_message","text":"Done."}}
{"type":"turn.completed"}`)
	seed := filepath.Join(t.TempDir(), "codex-auth.json")
	os.WriteFile(seed, []byte(`{"tokens":"first"}`), 0o600)
	c := &Codex{Command: bin, Dir: filepath.Join(t.TempDir(), "codex"), AuthFile: seed}
	var said []string
	session, err := c.Run(context.Background(), Turn{Dir: t.TempDir(), Prompt: "Do it.", System: "Rules.", Say: func(k, s string) { said = append(said, k+": "+s) }})
	if err != nil || session != "th-1" {
		t.Fatalf("session %q, %v", session, err)
	}
	args, env := read(t, rec+".args"), read(t, rec+".env")
	if !strings.HasPrefix(args, "exec\n--json\n") || !strings.Contains(args, `forced_login_method="chatgpt"`) || strings.Contains(args, "model_provider") ||
		!strings.Contains(args, "<harness-instructions>\nRules.\n</harness-instructions>\n\nDo it.") {
		t.Errorf("args:\n%s", args)
	}
	if !strings.Contains(env, "CODEX_HOME="+c.Dir) {
		t.Errorf("env:\n%s", env)
	}
	auth := filepath.Join(c.Dir, "auth.json")
	if got := read(t, auth); got != `{"tokens":"first"}` {
		t.Errorf("auth.json: %q", got)
	}
	if got := strings.Join(said, "|"); got != "tool: bash: harness status|result: bash: harness status\nIssue #1|text: Done." {
		t.Errorf("said %q", got)
	}
	// Codex refreshes its copy: the next turn keeps it.
	os.WriteFile(auth, []byte(`{"tokens":"refreshed"}`), 0o600)
	if _, err := c.Run(context.Background(), Turn{Dir: t.TempDir(), Session: session, Say: func(string, string) {}}); err != nil {
		t.Fatal(err)
	}
	if args := read(t, rec+".args"); !strings.HasPrefix(args, "exec\nresume\n") || !strings.Contains(args, "\nth-1\n") {
		t.Errorf("not resumed:\n%s", args)
	}
	if got := read(t, auth); got != `{"tokens":"refreshed"}` {
		t.Errorf("the refreshed sign-in was replaced: %q", got)
	}
	// A new sign-in replaces it.
	os.WriteFile(seed, []byte(`{"tokens":"second"}`), 0o600)
	if _, err := c.Run(context.Background(), Turn{Dir: t.TempDir(), Session: session, Say: func(string, string) {}}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, auth); got != `{"tokens":"second"}` {
		t.Errorf("the new sign-in was not used: %q", got)
	}
}

func TestLauncherAsTheBotsUser(t *testing.T) {
	l := Launcher{User: "harness-bot", Env: []string{"HARNESS_BOT_SOCKET=/s"}}
	argv, env := l.Command([]string{"opencode", "serve"}, []string{"OPENCODE_CONFIG=/c"})
	got := strings.Join(argv, " ")
	if !strings.HasPrefix(got, "sudo -n -u harness-bot --preserve-env=HARNESS_BOT_SOCKET,OPENCODE_CONFIG,SHELL -- /usr/bin/env PATH=") || !strings.HasSuffix(got, " opencode serve") {
		t.Errorf("argv: %s", got)
	}
	// The bots' user's login shell is nologin, which sudo would give it.
	if len(env) != 4 || !strings.HasPrefix(env[3], "SHELL=/bin/") {
		t.Errorf("env: %q", env)
	}
}
