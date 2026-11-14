package service

import (
	"context"
	_ "embed"
	"fmt"
	"github.com/michaelckearney/harness/contract"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/michaelckearney/harness/internal/agents"
	"github.com/michaelckearney/harness/internal/host"
	"github.com/michaelckearney/harness/internal/proxy"
	"github.com/michaelckearney/harness/internal/repo"
	"github.com/michaelckearney/harness/internal/store"
)

//go:embed instructions.md
var instructions string

// startAgents starts the proxy and sets up each agent this server runs.
// opencode and Claude Code reach their models only through the proxy,
// which holds the keys; what they hold is the proxy's token, which works
// only from this machine. Codex signs in with ChatGPT itself: see
// agents.Codex.
func (s *Service) startAgents(logger *log.Logger) error {
	c := s.cfg
	px, err := proxy.Start([]proxy.Route{
		{Name: "model", BaseURL: c.Model.BaseURL, KeyFile: c.Model.KeyFile},
		{Name: "anthropic", BaseURL: c.Anthropic.BaseURL, KeyFile: c.Anthropic.TokenFile, Beta: proxy.OAuthBeta},
	})
	if err != nil {
		return err
	}
	s.proxy = px
	logDir := filepath.Join(c.StateDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	env := []string{host.SocketEnv + "=" + c.Socket()}
	for _, k := range sortedKeys(c.Env) {
		env = append(env, k+"="+c.Env[k])
	}
	if c.User == "" {
		// The owner's gh login must not be the bots'.
		env = append(env, "GH_CONFIG_DIR="+filepath.Join(c.StateDir, "no-gh"))
	}
	if c.User != "" {
		// The worktrees are the service's, so git refuses them to the bots'
		// user as of dubious ownership. That user can reach only what it
		// is given, so it may trust every repository it can open.
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*")
	}
	policy := agents.Policy{Command: s.Command, ReadDirs: []string{filepath.Join(c.StateDir, "attachments")}}
	// The bots run this harness: its folder leads their PATH.
	path := os.Getenv("PATH")
	if exe, err := os.Executable(); err == nil {
		path = filepath.Dir(exe) + string(os.PathListSeparator) + path
	}
	s.agents = map[string]agents.Agent{}
	for _, name := range c.Agents {
		logFile, err := os.OpenFile(filepath.Join(logDir, "agent-"+name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		l := agents.Launcher{User: c.User, Env: env, Log: logFile, Path: path}
		dir := filepath.Join(c.StateDir, "agents", name)
		switch name {
		case repo.OpenCode:
			s.agents[name] = &agents.OpenCode{Command: c.Commands[name], Dir: dir, Policy: policy, Launcher: l,
				Model: agents.OpenCodeModel{ID: c.Model.ID, BaseURL: px.Base("model"), Key: px.Token, Vision: c.Model.Vision, Context: c.Model.Context}}
		case repo.ClaudeCode:
			s.agents[name] = &agents.ClaudeCode{Command: c.Commands[name], Dir: dir, Policy: policy, Launcher: l,
				BaseURL: px.Base("anthropic"), Key: px.Token}
		case repo.Codex:
			s.agents[name] = &agents.Codex{Command: c.Commands[name], Dir: dir, Policy: policy, Launcher: l,
				AuthFile: c.Codex.AuthFile}
		}
	}
	return nil
}

// stopAgents ends what the agents keep running, and the proxy.
func (s *Service) stopAgents() {
	for _, a := range s.agents {
		a.Stop()
	}
	if s.proxy != nil {
		s.proxy.Close()
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// system is the bot's instructions for a turn: the harness's, what the
// thread works on, then the instructions in the worktree for the
// framework and the scope: each folder's AGENTS.md, or its CLAUDE.md.
// They are read as files, never run: the worktree is the bot's.
func (s *Service) system(p *proj, t *store.Thread) string {
	sc := s.scopeOf(p, t.Scope)
	var b strings.Builder
	b.WriteString(strings.ReplaceAll(instructions, "$HARNESS", s.Command))
	fmt.Fprintf(&b, "\n## This issue\n\nYou work on %s. You may change files only in %s; harness commit refuses anything else.\n",
		sc, strings.Join(sc.Paths, ", "))
	dir := s.worktree(p, t.Number)
	var folders []string
	s.mu.Lock()
	cfg := p.repo
	s.mu.Unlock()
	if cfg != nil {
		folders = append(folders, cfg.Framework)
		if sc.Kind == repo.ProjectKind {
			folders = append(folders, sc.Path)
		}
		// The verbs as the worktree's framework.yaml has them: help, read
		// as text.
		if m, err := contract.LoadManifest(filepath.Join(dir, cfg.Framework)); err == nil {
			var verbs []string
			for _, v := range m.Names() {
				if v != contract.Test && m.InTurn(v) {
					verbs = append(verbs, fmt.Sprintf("- `harness %s`: %s", v, m.Verbs[v].Help))
				}
			}
			if len(verbs) > 0 {
				fmt.Fprintf(&b, "\nThe framework's verbs you may run:\n\n%s\n", strings.Join(verbs, "\n"))
			}
		}
	}
	for _, f := range folders {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			data, err := os.ReadFile(filepath.Join(dir, f, name))
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "\n## %s/%s\n\n%s\n", f, name, strings.TrimSpace(string(data)))
			break
		}
	}
	return b.String()
}

// agent runs one prompt in the thread's session, with the thread's agent.
func (s *Service) agent(ctx context.Context, p *proj, n int, prompt string) error {
	t, err := s.st.Thread(p.cfg.Repo, n)
	if err != nil {
		return err
	}
	a := s.agents[t.Agent]
	if a == nil {
		return fmt.Errorf("this server does not run agent %q", t.Agent)
	}
	logFile, err := s.threadLog(p, n)
	if err != nil {
		return err
	}
	defer logFile.Close()
	s.say(p, n, logFile, "prompt", fmt.Sprintf("=== %s %s session %s\n%s", time.Now().Format(time.RFC3339), t.Agent, t.Session, prompt))
	session, err := a.Run(ctx, agents.Turn{
		Dir:     s.worktree(p, n),
		Session: t.Session,
		Title:   fmt.Sprintf("gh-%d", n),
		Prompt:  prompt,
		System:  s.system(p, t),
		Say:     func(kind, text string) { s.say(p, n, logFile, kind, text) },
	})
	if session != "" && session != t.Session {
		if uerr := s.update(p.cfg.Repo, n, func(t *store.Thread) error { t.Session = session; return nil }); uerr != nil && err == nil {
			err = uerr
		}
	}
	return err
}

// say writes one line of a turn to the thread's log and the live view.
func (s *Service) say(p *proj, n int, out io.Writer, kind, text string) {
	if out != nil {
		fmt.Fprintf(out, "[%s] %s\n", kind, text)
	}
	s.live.publish(Line{Repo: p.cfg.Repo, Number: n, Time: time.Now(), Kind: kind, Text: text})
}
