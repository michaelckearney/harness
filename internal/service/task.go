package service

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/michaelckearney/harness/internal/discord"
	"github.com/michaelckearney/harness/internal/repo"
)

//go:embed task.md
var taskInstructions string

// taskCommands are the commands of a task's run, for the bot's help.
const taskCommands = "In a task's run, the commands are: issues, issue <n>, search-discord <words>, read-discord <link>, " +
	"open-issue -label <scope> -, done, and the framework's verbs for a turn, such as record -p <project>."

// beforeTimeout is how long a task's before may run.
const beforeTimeout = time.Hour

// taskKey keeps, for a task, the default branch's commit its last run opened
// no issue on: it runs again only once the default branch moves on.
func taskKey(repoName, task string) string { return "task:" + repoName + ":" + task }

// tasks starts a run of each of the monorepo's tasks that has room: fewer
// than its max_open open issues carry its label. A run waits in the queue
// Discord turns share, since it works on their checkout.
func (s *Service) tasks(p *proj) {
	s.mu.Lock()
	cfg, base := p.repo, p.base
	s.mu.Unlock()
	if cfg == nil || len(cfg.Tasks) == 0 {
		return
	}
	g, err := s.git(p, "")
	if err != nil {
		s.log.Printf("tasks: %v", err)
		return
	}
	head, err := g.Run("rev-parse", "refs/remotes/origin/"+base)
	if err != nil {
		s.log.Printf("tasks: %v", err)
		return
	}
	head = strings.TrimSpace(head)
	open := map[string]int{}
	counted := false
	for _, name := range cfg.TaskNames() {
		task := cfg.Tasks[name]
		s.mu.Lock()
		queued := s.queued[name]
		s.mu.Unlock()
		if queued || *task.MaxOpen == 0 {
			continue
		}
		if last, err := s.st.Get(taskKey(p.cfg.Repo, name)); err != nil || last == head {
			continue
		}
		if !counted {
			issues, err := s.gh.OpenIssues(p.cfg.Repo)
			if err != nil {
				s.log.Printf("tasks: %v", err)
				return
			}
			for _, i := range issues {
				if i.PullRequest != nil {
					continue
				}
				for _, l := range i.Labels {
					open[l.Name]++
				}
			}
			counted = true
		}
		if open[task.Label] >= *task.MaxOpen {
			continue
		}
		s.mu.Lock()
		if s.queued == nil {
			s.queued = map[string]bool{}
		}
		s.queued[name] = true
		s.mu.Unlock()
		name, task, n := name, task, open[task.Label]
		if !s.enqueue(func() {
			s.runTask(p, name, task, head, n)
			s.mu.Lock()
			delete(s.queued, name)
			s.mu.Unlock()
		}) {
			s.mu.Lock()
			delete(s.queued, name)
			s.mu.Unlock()
		}
	}
}

// runTask is one run of a task: the checkout at head, its before, then the
// agent with its prompt. A run that opens no issue, whatever the reason, is
// not tried again until the default branch moves on from head.
func (s *Service) runTask(p *proj, name string, task *repo.Task, head string, open int) {
	t := &talk{kind: "task", task: name, label: task.Label}
	defer func() {
		last := head
		if t.opened != 0 {
			last = ""
		}
		if err := s.st.Set(taskKey(p.cfg.Repo, name), last); err != nil {
			s.log.Printf("task %s: %v", name, err)
		}
	}()
	if err := s.prepareTalk(p); err != nil {
		s.log.Printf("task %s: checking out the default branch: %v", name, err)
		return
	}
	if task.Before != "" {
		if out, err := s.before(p, name, task.Before); err != nil {
			s.log.Printf("task %s: before failed: %v", name, err)
			s.taskFailed(name, task.Before, out, err)
			return
		}
	}
	prompt, err := os.ReadFile(filepath.Join(s.talkDir(p), task.Prompt))
	if err != nil {
		s.log.Printf("task %s: its prompt: %v", name, err)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This is a run of the monorepo's %s task. It runs because fewer than %d open issues carry its label, %s: %d do.\n",
		name, *task.MaxOpen, task.Label, open)
	if task.Before != "" {
		fmt.Fprintf(&b, "Before you started, `%s` ran in your folder and passed; what it leaves there is yours to read.\n", task.Before)
	}
	fmt.Fprintf(&b, "\nThe task, from %s:\n\n%s\n\nOpen at most one issue, with `%s open-issue -label <scope> -`, or none if nothing is worth it. Then end the turn with `%s done`.\n",
		task.Prompt, strings.TrimSpace(string(prompt)), s.Command, s.Command)
	if _, err := s.talkTurn(p, t, "", fmt.Sprintf("task-%s-%s", name, time.Now().Format("2006-01-02-1504")), b.String()); err != nil {
		s.log.Printf("task %s: %v", name, err)
	}
	if t.opened == 0 {
		s.log.Printf("task %s: opened no issue; it runs again when %s moves on", name, p.base)
	}
}

// before runs a task's before on the checkout, with the host's environment,
// as prepare does a thread's, and returns the end of what it printed.
func (s *Service) before(p *proj, name, command string) (string, error) {
	logDir := filepath.Join(s.cfg.StateDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", err
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, fmt.Sprintf("%s-task-%s.log", strings.ReplaceAll(p.cfg.Repo, "/", "-"), name)),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n=== %s before\n$ %s\n", time.Now().Format(time.RFC3339), command)
	ctx, cancel := context.WithTimeout(context.Background(), beforeTimeout)
	defer cancel()
	var out tail
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = s.talkDir(p)
	w := io.MultiWriter(logFile, &out)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	err = cmd.Run()
	return out.String(), err
}

// taskFailed says in Discord that a task's before failed, so the owner
// hears of it: the task does not run again until the default branch moves
// on.
func (s *Service) taskFailed(name, command, out string, err error) {
	if s.chat == nil {
		return
	}
	text := fmt.Sprintf("`%s` failed on the default branch (%v), so the task did not run. It runs again when the default branch moves on.\n\n```\n%s\n```",
		command, err, strings.ReplaceAll(strings.TrimSpace(out), "```", "'''"))
	m := discord.Message{Title: fmt.Sprintf("⚠️ The %s task did not run", name), Description: truncate(text, discord.Limit), Color: red}
	if _, err := s.chat.Post(digestChannel, m); err != nil {
		s.log.Printf("discord: task %s: %v", name, err)
	}
}

// taskLabels are the labels of the issue a task's run opens: exactly one
// scope, so a bot works it at once, and the task's own label.
func (s *Service) taskLabels(p *proj, t *talk, labels []string) ([]string, error) {
	if t.opened != 0 {
		return nil, fixable(fmt.Sprintf("this run already opened #%d, and a run opens one issue at most; end the turn with done", t.opened))
	}
	s.mu.Lock()
	cfg := p.repo
	s.mu.Unlock()
	var scopes []string
	for _, l := range labels {
		if l == t.label {
			continue
		}
		if cfg == nil {
			return nil, fmt.Errorf("harness.yaml is not read yet")
		}
		if _, err := cfg.ScopeNamed(l); err != nil {
			return nil, fixable(fmt.Sprintf("%q is not a scope label; a task's issue gets one scope label, project:<name> or framework, and the harness adds %s", l, t.label))
		}
		scopes = append(scopes, l)
	}
	if len(scopes) != 1 {
		return nil, fixable("give the issue exactly one scope label, with -label project:<name> or -label framework, so a bot works it")
	}
	return []string{scopes[0], t.label}, nil
}

// tail keeps the end of what a command prints.
type tail struct{ b []byte }

const tailSize = 3000

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailSize {
		t.b = t.b[len(t.b)-tailSize:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(bytes.ToValidUTF8(t.b, nil)) }
