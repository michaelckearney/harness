package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/michaelckearney/harness/internal/agents/opencode"
)

// OpenCodeModel is the OpenAI-compatible model opencode runs on, reached
// through the proxy.
type OpenCodeModel struct {
	ID      string
	BaseURL string // the proxy's route to the endpoint
	Key     string // the proxy's token
	Vision  bool
	Context int
}

// OpenCode runs turns in `opencode serve`, one server for the service, one
// session per thread.
type OpenCode struct {
	Command  string
	Dir      string // where its configuration goes
	Model    OpenCodeModel
	Policy   Policy
	Launcher Launcher

	mu     sync.Mutex
	server *opencode.Server
}

const provider = "harness"

func (o *OpenCode) Name() string { return "opencode" }

// rules are what the bot may do, for each session. The last rule that
// matches decides. Everything is allowed but:
//
//   - shell commands other than the harness. The bot reads and edits files
//     with opencode's own tools. A bot whose shell could run anything spent
//     its turn on journalctl, strings, and its own systemctl restart.
//   - files outside the worktree, but for the folders the policy names and
//     opencode's own copy of a command's long output.
//   - asking a question, which would wait for an answer nobody is there to
//     give.
func (o *OpenCode) rules() []opencode.Rule {
	cmd := o.Policy.Command
	rules := []opencode.Rule{
		{Permission: "*", Pattern: "*", Action: "allow"},
		{Permission: "bash", Pattern: "*", Action: "deny"},
		{Permission: "bash", Pattern: cmd, Action: "allow"},
		{Permission: "bash", Pattern: cmd + " *", Action: "allow"},
		{Permission: "external_directory", Pattern: "*", Action: "deny"},
		{Permission: "external_directory", Pattern: "*/opencode/tool-output/*", Action: "allow"},
		{Permission: "question", Pattern: "*", Action: "deny"},
	}
	for _, d := range o.Policy.ReadDirs {
		rules = append(rules, opencode.Rule{Permission: "external_directory", Pattern: d + "/*", Action: "allow"})
	}
	return rules
}

// permission is rules as opencode.json writes them.
func permission(rules []opencode.Rule) map[string]any {
	patterns := map[string]map[string]any{}
	for _, r := range rules {
		if r.Permission == "*" {
			continue
		}
		if patterns[r.Permission] == nil {
			patterns[r.Permission] = map[string]any{}
		}
		patterns[r.Permission][r.Pattern] = r.Action
	}
	out := map[string]any{}
	for name, m := range patterns {
		if a, ok := m["*"]; ok && len(m) == 1 {
			out[name] = a
		} else {
			out[name] = m
		}
	}
	return out
}

// config is opencode.json: the model, and the same rules as each session's.
func (o *OpenCode) config() ([]byte, error) {
	m := o.Model
	model := map[string]any{"name": m.ID, "tool_call": true}
	if m.Vision {
		model["attachment"] = true
		model["modalities"] = map[string]any{"input": []string{"text", "image"}, "output": []string{"text"}}
	}
	if m.Context > 0 {
		model["limit"] = map[string]any{"context": m.Context, "output": m.Context / 4}
	}
	config := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"autoupdate": false,
		"share":      "disabled",
		"model":      provider + "/" + m.ID,
		"provider": map[string]any{
			provider: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "harness",
				// No request timeout: a model gateway holds a request while
				// it loads a model, which takes minutes.
				"options": map[string]any{"baseURL": m.BaseURL, "apiKey": m.Key, "timeout": false},
				"models":  map[string]any{m.ID: model},
			},
		},
		"permission": permission(o.rules()),
	}
	data, err := json.MarshalIndent(config, "", "  ")
	return append(data, '\n'), err
}

func (o *OpenCode) start() (*opencode.Server, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.server != nil && o.server.Alive() {
		return o.server, nil
	}
	if o.server != nil {
		o.server.Stop()
	}
	data, err := o.config()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Dir, 0o770); err != nil {
		return nil, err
	}
	path := filepath.Join(o.Dir, "opencode.json")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return nil, err
	}
	// The password goes through the launcher, so sudo keeps it.
	pw := opencode.NewPassword()
	argv, env := o.Launcher.Command([]string{o.Command}, []string{"OPENCODE_CONFIG=" + path, opencode.PasswordEnv + "=" + pw})
	srv, err := opencode.Start(pw, argv, env, o.Launcher.Log)
	if err != nil {
		return nil, err
	}
	o.server = srv
	return srv, nil
}

func (o *OpenCode) Stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.server != nil {
		o.server.Stop()
		o.server = nil
	}
}

func (o *OpenCode) Run(ctx context.Context, t Turn) (string, error) {
	srv, err := o.start()
	if err != nil {
		return t.Session, err
	}
	session := t.Session
	if session != "" {
		if err := srv.Session(ctx, t.Dir, session); errors.Is(err, opencode.ErrNoSession) {
			// A session deleted by hand would fail every turn: start afresh.
			t.Say("error", fmt.Sprintf("session %s is gone; starting a new one", session))
			session = ""
		} else if err != nil {
			return session, err
		}
	}
	if session == "" {
		if session, err = srv.CreateSession(ctx, t.Dir, t.Title, o.rules()); err != nil {
			return "", err
		}
	}
	watch, stop := context.WithCancel(ctx)
	defer stop()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		o.watch(watch, srv, t, session)
	}()
	err = srv.Prompt(ctx, t.Dir, session, opencode.Model{ProviderID: provider, ModelID: o.Model.ID}, t.System, t.Prompt)
	// The last parts arrive just after the reply: give them a moment.
	time.Sleep(200 * time.Millisecond)
	stop()
	<-watched
	return session, err
}

// watch follows a session's events until ctx ends.
func (o *OpenCode) watch(ctx context.Context, srv *opencode.Server, t Turn, session string) {
	roles := map[string]string{} // message id → user or assistant
	shown := map[string]string{} // part id → the state last shown
	for ctx.Err() == nil {
		err := srv.Events(ctx, t.Dir, func(e opencode.Event) {
			switch e.Type {
			case "message.updated":
				var m struct {
					Info struct {
						ID, SessionID, Role string
					} `json:"info"`
				}
				if json.Unmarshal(e.Properties, &m) == nil && m.Info.SessionID == session {
					roles[m.Info.ID] = m.Info.Role
				}
			case "message.part.updated":
				var m struct {
					Part opencode.Part `json:"part"`
				}
				if json.Unmarshal(e.Properties, &m) != nil || m.Part.SessionID != session || roles[m.Part.MessageID] == "user" {
					return
				}
				kind, text := describe(m.Part)
				if text == "" || shown[m.Part.ID] == kind+text {
					return
				}
				shown[m.Part.ID] = kind + text
				t.Say(kind, text)
			case "session.error":
				var m struct {
					SessionID string          `json:"sessionID"`
					Error     json.RawMessage `json:"error"`
				}
				if json.Unmarshal(e.Properties, &m) == nil && m.SessionID == session {
					t.Say("error", string(m.Error))
				}
			}
		})
		if err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

// describe says what a part shows, once it has something to show.
func describe(part opencode.Part) (kind, text string) {
	switch part.Type {
	case "text":
		if part.Time.End > 0 {
			return "text", strings.TrimSpace(part.Text)
		}
	case "reasoning":
		if part.Time.End > 0 {
			return "reasoning", strings.TrimSpace(part.Text)
		}
	case "tool":
		st := part.State
		title := st.Title
		if title == "" {
			title = compact(string(st.Input), 200)
		}
		switch st.Status {
		case "running":
			return "tool", part.Tool + ": " + title
		case "completed":
			return "result", part.Tool + ": " + title + "\n" + compact(st.Output, 2000)
		case "error":
			return "error", part.Tool + ": " + title + "\n" + st.Error
		}
	}
	return "", ""
}
