package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealServer runs the opencode on PATH, when OPENCODE_REAL is set: the
// fakes in the service's tests only show harness serve agrees with itself.
func TestRealServer(t *testing.T) {
	if os.Getenv("OPENCODE_REAL") == "" {
		t.Skip("set OPENCODE_REAL=1 to run against the opencode on PATH")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "opencode.json")
	// A model nobody serves: the prompt must come back with an error, not hang.
	os.WriteFile(config, []byte(`{"autoupdate":false,"share":"disabled","model":"slipway/none",
"provider":{"slipway":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"http://127.0.0.1:9/v1"},"models":{"none":{"name":"none"}}}},
"permission":"allow"}`), 0o600)
	pw := NewPassword()
	srv, err := Start(pw, []string{"opencode"}, append(os.Environ(), "OPENCODE_CONFIG="+config, PasswordEnv+"="+pw), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	ctx := context.Background()
	if err := srv.Session(ctx, dir, "ses_nothere"); !errors.Is(err, ErrNoSession) {
		t.Errorf("a missing session: %v", err)
	}
	id, err := srv.CreateSession(ctx, dir, "gh-1", []Rule{{Permission: "*", Pattern: "*", Action: "allow"}, {Permission: "question", Pattern: "*", Action: "deny"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Session(ctx, dir, id); err != nil {
		t.Errorf("the new session: %v", err)
	}
	events := make(chan Event, 1000)
	ectx, stop := context.WithCancel(ctx)
	go srv.Events(ectx, dir, func(e Event) { events <- e })
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err = srv.Prompt(pctx, dir, id, Model{ProviderID: "slipway", ModelID: "none"}, "", "Say hi.")
	t.Logf("prompt: %v", err)
	if pctx.Err() != nil {
		t.Fatal("the prompt hung on a model nobody serves")
	}
	time.Sleep(300 * time.Millisecond)
	stop()
	close(events)
	seen := map[string]bool{}
	for e := range events {
		seen[e.Type] = true
		if e.Type == "message.part.updated" {
			var m struct{ Part Part }
			json.Unmarshal(e.Properties, &m)
			t.Logf("part %s %s %q", m.Part.Type, m.Part.State.Status, m.Part.Text)
		}
	}
	t.Logf("events: %v", seen)
	if !seen["message.updated"] {
		t.Error("no message.updated events")
	}
}
