package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/michaelckearney/harness/internal/store"
)

// What the API shows. Nothing here is GitHub's: the desktop app asks GitHub
// itself, with the owner's gh, for issues and pull requests.

// ProjectView is the monorepo the service works.
type ProjectView struct {
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Runner string `json:"runner"` // the agents this server runs
	// Projects are the monorepo's, from harness.yaml.
	Projects []string `json:"projects"`
	// Ready is false until the default branch has a valid harness.yaml.
	Ready   bool `json:"ready"`
	Threads int  `json:"threads"`
	Running int  `json:"running"`
}

// ThreadView is one open thread.
type ThreadView struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	State  string `json:"state"`
	// Agent works the thread; Scope is what on, such as project:harbor,
	// or "" for the whole monorepo.
	Agent   string    `json:"agent"`
	Scope   string    `json:"scope"`
	PR      int       `json:"pr"`
	Session string    `json:"session"`
	Started time.Time `json:"started"`
	Waiting int       `json:"waiting"` // events no turn has answered yet
}

// Projects lists the projects and how many threads each has.
func (s *Service) Projects() ([]ProjectView, error) {
	threads, err := s.st.Threads()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ProjectView
	for _, p := range s.projects {
		v := ProjectView{Repo: p.cfg.Repo, Base: p.base, Runner: strings.Join(s.cfg.Agents, ","), Ready: p.repo != nil, Projects: []string{}}
		if p.repo != nil {
			v.Projects = p.repo.ProjectNames()
		}
		for _, t := range threads {
			if t.Repo == p.cfg.Repo && t.State != store.Closed {
				v.Threads++
				if t.State == store.Running {
					v.Running++
				}
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// Threads lists the open threads, of one repository or of all of them.
func (s *Service) Threads(repo string) ([]ThreadView, error) {
	threads, err := s.st.Threads()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ThreadView{}
	for _, t := range threads {
		if t.State == store.Closed || (repo != "" && t.Repo != repo) {
			continue
		}
		v := ThreadView{Repo: t.Repo, Number: t.Number, State: t.State, Agent: t.Agent, Scope: t.Scope, PR: t.PR, Session: t.Session, Started: t.Started}
		if p := s.project(t.Repo); p != nil {
			pending, _ := s.pending(p, t.Number)
			v.Waiting = unbatched(pending, t.Batch)
		}
		out = append(out, v)
	}
	return out, nil
}

// ErrNoThread is returned for a thread the service does not have open.
var ErrNoThread = errors.New("no such open thread")

// Thread returns one open thread.
func (s *Service) Thread(repo string, n int) (ThreadView, error) {
	threads, err := s.Threads(repo)
	if err != nil {
		return ThreadView{}, err
	}
	for _, t := range threads {
		if t.Number == n {
			return t, nil
		}
	}
	return ThreadView{}, ErrNoThread
}

// Lines is what the live view holds of a thread: the end of its turns.
func (s *Service) Lines(repo string, n int) []Line { return s.live.lines(repo, n) }

// Subscribe returns every line from now on, and a function that ends it.
func (s *Service) Subscribe() (<-chan Line, func()) { return s.live.subscribe() }

// Log returns the end of a thread's log: every turn's prompt and what the
// bot did, up to max bytes.
func (s *Service) Log(repo string, n int, max int64) (string, error) {
	p := s.project(repo)
	if p == nil {
		return "", ErrNoThread
	}
	path := filepath.Join(s.cfg.StateDir, "logs", fmt.Sprintf("%s-%d.log", strings.ReplaceAll(repo, "/", "-"), n))
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	from := st.Size() - max
	if from < 0 {
		from = 0
	}
	buf := make([]byte, st.Size()-from)
	if _, err := f.ReadAt(buf, from); err != nil {
		return "", err
	}
	return string(buf), nil
}

// Stop stops a thread's running turn. Its events stay unanswered, so the
// thread waits for something new rather than starting again at once.
func (s *Service) Stop(repo string, n int) error {
	s.mu.Lock()
	cancel := s.turns[key(repo, n)]
	s.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("%s has no turn running", key(repo, n))
	}
	s.log.Printf("%s: turn stopped by the owner", key(repo, n))
	cancel()
	return nil
}

// Retry gives a blocked or idle thread another turn, as if the owner had
// commented asking for one.
func (s *Service) Retry(repo string, n int) error {
	p := s.project(repo)
	if p == nil {
		return ErrNoThread
	}
	t, err := s.st.Thread(repo, n)
	if err != nil {
		return err
	}
	if t == nil || t.State == store.Closed {
		return ErrNoThread
	}
	if t.State == store.Running {
		return fmt.Errorf("%s is running a turn already", key(repo, n))
	}
	now := time.Now()
	e := store.Event{Key: fmt.Sprintf("retry:%d", now.UnixNano()), Kind: "retry", At: now,
		Text: "The owner asked you to try again: look at where you stopped, and carry on."}
	s.mu.Lock()
	s.retries[n] = append(s.retries[n], retry{repo: repo, event: e})
	s.mu.Unlock()
	if err := s.update(repo, n, func(t *store.Thread) error { t.State = store.Idle; return nil }); err != nil {
		return err
	}
	s.log.Printf("%s: retried by the owner", key(repo, n))
	s.nudge()
	return nil
}

// retry is an event the owner asked for from the desktop app. GitHub has no
// record of it, so a poll would not bring it back: it is kept here until a
// turn answers it.
type retry struct {
	repo  string
	event store.Event
}
