package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/michaelckearney/harness/internal/store"
)

// start begins a turn on a batch of events. The slot is already taken.
func (s *Service) start(ctx context.Context, p *proj, t *store.Thread, batch []store.Event) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.turns[key(t.Repo, t.Number)] = cancel
	s.mu.Unlock()
	err := s.update(t.Repo, t.Number, func(t *store.Thread) error {
		t.State, t.Batch, t.Outcome, t.Nudged, t.Started, t.Replied = store.Running, batch, "", false, time.Now(), false
		return nil
	})
	if err != nil {
		s.log.Printf("%s: %v", key(t.Repo, t.Number), err)
	}
	s.log.Printf("%s: turn started, %d event(s)", key(t.Repo, t.Number), len(batch))
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.turns, key(t.Repo, t.Number))
			s.mu.Unlock()
			<-s.slots
			s.nudge()
		}()
		if err := s.turn(ctx, p, t.Number); err != nil {
			s.log.Printf("%s: %v", key(t.Repo, t.Number), err)
		}
	}()
}

// turn runs the agent until the bot ends the turn with done, resuming it
// once if it stops without.
func (s *Service) turn(ctx context.Context, p *proj, n int) error {
	repo := p.cfg.Repo
	if err := s.ensureWorktree(p, n); err != nil {
		return s.cannotStart(repo, n, err)
	}
	// Where the branch was when the turn started: done asks for the tests
	// only when the turn committed.
	if g, err := s.wgit(p, &store.Thread{Repo: repo, Number: n}); err == nil {
		if head, err := g.Run("rev-parse", "HEAD"); err == nil {
			s.update(repo, n, func(t *store.Thread) error { t.TurnHead = head; return nil })
		}
	}
	t, err := s.st.Thread(repo, n)
	if err != nil {
		return err
	}
	// A pull request marked ready goes back to draft while the bot works on
	// what you said about it.
	if t.PR != 0 {
		if pr, err := s.gh.PR(repo, t.PR); err == nil && pr.State == "open" && !pr.Draft {
			if err := s.gh.SetDraft(repo, pr.NodeID, true); err != nil {
				s.log.Printf("%s: back to draft: %v", key(repo, n), err)
			}
		}
	}

	prompt := s.prompt(p, t)
	if err := s.prepare(ctx, p, n); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		s.log.Printf("%s: prepare: %v", key(repo, n), err)
		prompt += fmt.Sprintf("\nthe harness could not prepare your worktree: `%s` failed. If a command fails because of it, say so with `harness reply`; "+
			"you cannot fix it from here.\n", s.prepareCmd(p))
	}
	prompts := []string{prompt, s.nudgePrompt()}
	for i, prompt := range prompts {
		if err := s.agent(ctx, p, n, prompt); err != nil {
			if ctx.Err() != nil {
				return nil // the thread closed under it
			}
			s.log.Printf("%s: agent: %v", key(repo, n), err)
		}
		t, err := s.st.Thread(repo, n)
		if err != nil {
			return err
		}
		if t.State != store.Running {
			return nil
		}
		if t.Outcome != "" {
			s.log.Printf("%s: turn ended: %s", key(repo, n), t.Outcome)
			if err := s.st.Handle(repo, n, t.Batch); err != nil {
				return err
			}
			return s.update(repo, n, func(t *store.Thread) error {
				t.State, t.Batch, t.Nudged = store.Idle, nil, false
				return nil
			})
		}
		if i == 0 {
			s.log.Printf("%s: turn ended without done; resuming it once", key(repo, n))
			if err := s.update(repo, n, func(t *store.Thread) error { t.Nudged = true; return nil }); err != nil {
				return err
			}
		}
	}

	// Twice without a word. The events are answered, by this, so the thread
	// waits for something new rather than going round again.
	s.log.Printf("%s: blocked", key(repo, n))
	if err := s.st.Handle(repo, n, t.Batch); err != nil {
		return err
	}
	if err := s.update(repo, n, func(t *store.Thread) error {
		t.State, t.Batch, t.Nudged = store.Blocked, nil, false
		return nil
	}); err != nil {
		return err
	}
	return s.gh.Comment(repo, n, "harness: the bot stopped twice without ending its turn, so this thread is blocked. "+
		"Comment here to give it another try.")
}

// cannotStart blocks a thread whose turn could not start, and says why in
// the issue. Put back in the queue, it started again at once, and failed
// the same way every few seconds (monorepo#17).
func (s *Service) cannotStart(repo string, n int, cause error) error {
	s.log.Printf("%s: blocked: %v", key(repo, n), cause)
	t, err := s.st.Thread(repo, n)
	if err != nil {
		return err
	}
	if err := s.st.Handle(repo, n, t.Batch); err != nil {
		return err
	}
	if err := s.update(repo, n, func(t *store.Thread) error {
		t.State, t.Batch, t.Nudged = store.Blocked, nil, false
		return nil
	}); err != nil {
		return err
	}
	return s.gh.Comment(repo, n, fmt.Sprintf("harness: the turn could not start (%v), so this thread is blocked. "+
		"Comment here to give it another try once that is fixed.", cause))
}

func (s *Service) prompt(p *proj, t *store.Thread) string {
	issue, err := s.gh.Issue(t.Repo, t.Number)
	title := ""
	if err == nil {
		title = ": " + issue.Title
	}
	var b strings.Builder
	fmt.Fprintf(&b, "New activity on %s issue #%d%s\n\n", t.Repo, t.Number, title)
	for _, e := range t.Batch {
		line := strings.Join(strings.Fields(e.Text), " ")
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		fmt.Fprintf(&b, "- %s\n", line)
	}
	fmt.Fprintf(&b, "\nStart with `harness status`, which shows these in full. Answer with `harness reply`, then end this turn with `harness done`.\n")
	fmt.Fprintf(&b, "An image someone attached shows as the path of a copy on this machine: look at it with your read tool.\n")
	return b.String()
}

func (s *Service) nudgePrompt() string {
	return "Your turn ended without `harness done`, so nobody knows what happened. " +
		"Run `harness status`, finish what you were doing, answer with `harness reply`, and end the turn with `harness done`."
}

// threadLog opens the log of a thread's turns, to append to.
func (s *Service) threadLog(p *proj, n int) (*os.File, error) {
	logDir := filepath.Join(s.cfg.StateDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(logDir, fmt.Sprintf("%s-%d.log", strings.ReplaceAll(p.cfg.Repo, "/", "-"), n)),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// prepareCmd is harness.yaml's prepare, as the default branch has it.
func (s *Service) prepareCmd(p *proj) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.repo == nil {
		return ""
	}
	return p.repo.Prepare
}

// prepare runs the monorepo's prepare command in the worktree, with the
// host's environment, unlike the bot's.
func (s *Service) prepare(ctx context.Context, p *proj, n int) error {
	command := s.prepareCmd(p)
	if command == "" {
		return nil
	}
	logFile, err := s.threadLog(p, n)
	if err != nil {
		return err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n=== %s prepare\n$ %s\n", time.Now().Format(time.RFC3339), command)
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = s.worktree(p, n)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	return cmd.Run()
}

// ensureWorktree gives a thread its worktree, on its branch: from GitHub if
// the branch was pushed, or else new from the default branch.
func (s *Service) ensureWorktree(p *proj, n int) error {
	if err := s.addWorktree(p, n); err != nil {
		return err
	}
	return s.setupPRFile(p, n)
}

// addWorktree checks out the thread's branch in its worktree, unless it is
// there already.
func (s *Service) addWorktree(p *proj, n int) error {
	path := s.worktree(p, n)
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return nil
	}
	g, err := s.git(p, "")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), s.dirMode()); err != nil {
		return err
	}
	g.Run("worktree", "prune")
	s.mu.Lock()
	from := "refs/remotes/origin/" + p.base
	s.mu.Unlock()
	if _, err := g.Run("rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch(n)); err == nil {
		from = "refs/remotes/origin/" + branch(n)
	}
	_, err = g.Run("worktree", "add", "--quiet", "-B", branch(n), path, from)
	return err
}
