package service

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/michaelckearney/harness/internal/agents"
	"github.com/michaelckearney/harness/internal/host"
	"github.com/michaelckearney/harness/internal/repo"
)

// talker is an agent that takes a Discord turn as the test says.
type talker struct {
	name  string
	runs  []string // the prompts it was given
	limit bool     // it is at its limit
	act   func()   // what it does before it stops, if anything
}

func (a *talker) Name() string { return a.name }
func (a *talker) Stop()        {}
func (a *talker) Run(ctx context.Context, t agents.Turn) (string, error) {
	a.runs = append(a.runs, t.Prompt)
	if a.act != nil {
		a.act()
	}
	if a.limit {
		return t.Session, &agents.LimitError{Msg: a.name + ": usage limit", Until: time.Now().Add(time.Hour)}
	}
	if t.Session != "" {
		return t.Session, nil
	}
	return a.name + "-session", nil
}

func talkService(t *testing.T, talkers ...*talker) (*Service, *proj) {
	s := &Service{cfg: &host.Config{StateDir: t.TempDir()}, log: log.New(io.Discard, "", 0),
		agents: map[string]agents.Agent{}, limits: map[string]time.Time{}, Command: "harness"}
	var names []string
	for _, a := range talkers {
		s.agents[a.name] = a
		names = append(names, a.name)
	}
	p := &proj{cfg: &projCfg{Repo: "o/r"}, repo: &repo.Config{Agents: repo.Agents{Talk: names}}}
	return s, p
}

func prompt(resume bool) string {
	if resume {
		return "since"
	}
	return "all"
}

func TestTheNextTalkAgentTakesATurnAtTheLimit(t *testing.T) {
	codex := &talker{name: repo.Codex, limit: true}
	claude := &talker{name: repo.ClaudeCode}
	s, p := talkService(t, codex, claude)

	agent, session, err := s.talkWith(p, &talk{}, "", "", "x", prompt)
	if err != nil || agent != repo.ClaudeCode || session != "claude-code-session" {
		t.Fatalf("the turn: %s %s %v", agent, session, err)
	}
	if len(codex.runs) != 1 || len(claude.runs) != 1 || claude.runs[0] != "all" {
		t.Errorf("codex ran %q, claude %q", codex.runs, claude.runs)
	}
	// While Codex is at its limit, Claude goes first, and goes on with its
	// session.
	agent, _, _ = s.talkWith(p, &talk{}, agent, session, "x", prompt)
	if agent != repo.ClaudeCode || len(codex.runs) != 1 || claude.runs[1] != "since" {
		t.Errorf("the next turn: %s; codex ran %d times, claude %q", agent, len(codex.runs), claude.runs)
	}
	// Once the limit resets, a conversation stays with its agent, and a new
	// one starts with the first.
	s.limits[repo.Codex] = time.Time{}
	codex.limit = false
	if agent, _, _ = s.talkWith(p, &talk{}, repo.ClaudeCode, session, "x", prompt); agent != repo.ClaudeCode {
		t.Errorf("a conversation Claude has: %s", agent)
	}
	if agent, _, _ = s.talkWith(p, &talk{}, "", "", "x", prompt); agent != repo.Codex {
		t.Errorf("a new conversation: %s", agent)
	}
}

func TestATurnThatActedIsNotTakenAgain(t *testing.T) {
	tk := &talk{}
	codex := &talker{name: repo.Codex, limit: true, act: func() { tk.acted = true }}
	claude := &talker{name: repo.ClaudeCode}
	s, p := talkService(t, codex, claude)
	agent, _, err := s.talkWith(p, tk, "", "", "x", prompt)
	if err == nil || agent != repo.Codex || len(claude.runs) != 0 {
		t.Errorf("%s %v; claude ran %d times", agent, err, len(claude.runs))
	}
}

func TestEveryTalkAgentAtItsLimit(t *testing.T) {
	codex := &talker{name: repo.Codex, limit: true}
	claude := &talker{name: repo.ClaudeCode, limit: true}
	s, p := talkService(t, codex, claude)
	if _, _, err := s.talkWith(p, &talk{}, "", "", "x", prompt); err == nil {
		t.Error("no error")
	}
	// The limits are guesses: the next turn tries them again, in order.
	s.talkWith(p, &talk{}, "", "", "x", prompt)
	if len(codex.runs) != 2 || len(claude.runs) != 2 {
		t.Errorf("codex ran %d times, claude %d", len(codex.runs), len(claude.runs))
	}
}
