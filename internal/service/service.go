// Package service is `harness serve`: it watches one monorepo on GitHub,
// keeps the bots' configuration in step with its default branch's
// harness.yaml, runs one turn at a time per thread with the thread's agent,
// and answers the bots' commands.
package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/michaelckearney/harness/internal/agents"
	"github.com/michaelckearney/harness/internal/git"
	"github.com/michaelckearney/harness/internal/github"
	"github.com/michaelckearney/harness/internal/host"
	"github.com/michaelckearney/harness/internal/proxy"
	"github.com/michaelckearney/harness/internal/repo"
	"github.com/michaelckearney/harness/internal/store"
)

type Service struct {
	cfg *host.Config
	gh  *github.Client
	st  *store.Store
	log *log.Logger
	gid int // the group shared with the bots' user, or -1

	mu       sync.Mutex
	projects []*proj
	turns    map[string]context.CancelFunc // running turns, by thread key
	slots    chan struct{}
	wake     chan struct{}
	live     *hub
	retries  map[int][]retry // by issue number
	draining bool            // stopping: no new turns start
	socket   os.FileInfo     // the socket file this service made
	agents   map[string]agents.Agent
	proxy    *proxy.Proxy
	// Command is the harness, as the bots run it: the one command their
	// shell may run.
	Command string
	// ConfigPath is the host configuration's file, which a harness the
	// service updates itself to must accept. Without it, it never updates.
	ConfigPath string
	tried      map[string]time.Time // the versions selfUpdate tried, by when
}

// projCfg is the monorepo, as the host configuration has it.
type projCfg struct {
	Repo, Name, GitURL string
}

// proj is what the service knows about the monorepo between polls.
type proj struct {
	cfg   *projCfg
	base  string       // the default branch
	tree  string       // the harness.yaml blob repo was last read from
	repo  *repo.Config // nil until a valid harness.yaml is read
	pin   string       // the harness harness.yaml pins, valid or not
	inbox map[int][]store.Event
}

func New(cfg *host.Config, logger *log.Logger) (*Service, error) {
	s := &Service{cfg: cfg, gid: -1}
	if err := s.prepareStateDir(); err != nil {
		return nil, err
	}
	db := filepath.Join(cfg.StateDir, "harness.db")
	st, err := store.Open(db)
	if err != nil {
		return nil, err
	}
	*s = Service{
		cfg:     cfg,
		gid:     s.gid,
		gh:      &github.Client{API: cfg.GitHub.APIURL, AppID: cfg.GitHub.AppID, KeyFile: cfg.GitHub.PrivateKey, TokenFile: cfg.GitHub.TokenFile},
		st:      st,
		log:     logger,
		turns:   map[string]context.CancelFunc{},
		slots:   make(chan struct{}, cfg.Sessions),
		wake:    make(chan struct{}, 1),
		live:    newHub(),
		retries: map[int][]retry{},
		Command: "harness",
	}
	s.projects = []*proj{{cfg: &projCfg{Repo: cfg.Repo, Name: cfg.Name, GitURL: cfg.GitURL}, inbox: map[int][]store.Event{}}}
	if err := s.startAgents(logger); err != nil {
		st.Close()
		return nil, err
	}
	return s, nil
}

// Run polls and schedules until ctx ends.
//
// When ctx ends, no new turn starts, and the turns already running finish.
// When stop closes as well, they are stopped where they are: a turn that is
// stopped is answered again on the next start, so that loses only the turn.
func (s *Service) Run(ctx context.Context, stop <-chan struct{}) error {
	defer s.st.Close()
	defer s.stopAgents()
	// A turn that was running when the service stopped never ended: its
	// events are still unanswered, so the next poll queues it again.
	threads, err := s.st.Threads()
	if err != nil {
		return err
	}
	for _, t := range threads {
		if t.State == store.Running {
			t.State, t.Outcome = store.Idle, ""
			if err := s.st.Save(t); err != nil {
				return err
			}
		}
	}
	ln, err := s.listen()
	if err != nil {
		return err
	}
	defer func() { s.unlisten(ln) }()
	go s.serve(ln)

	// Turns outlive ctx: when it ends they are left to finish (see drain).
	turns, endTurns := context.WithCancel(context.Background())
	defer endTurns()
	s.log.Printf("serve: watching %d project(s), polling every %s", len(s.projects), s.cfg.Poll)
	tick := time.NewTicker(s.cfg.Poll)
	defer tick.Stop()
	for {
		for _, p := range s.projects {
			if err := s.poll(ctx, p); err != nil {
				s.log.Printf("%s: %v", p.cfg.Repo, err)
			}
		}
		if s.selfUpdate(ctx) {
			s.drain(stop)
			return ErrUpdated
		}
		s.schedule(turns)
		select {
		case <-ctx.Done():
			s.drain(stop)
			return nil
		case <-tick.C:
			ln = s.keepSocket(ln)
		case <-s.wake:
			// A turn ended: start whatever was waiting on it, without asking
			// GitHub again.
			s.schedule(turns)
			continue
		}
	}
}

// drain waits for the running turns to end, or for stop to close, and then
// stops whatever is still running.
func (s *Service) drain(stop <-chan struct{}) {
	s.mu.Lock()
	s.draining = true
	n := len(s.turns)
	s.mu.Unlock()
	if n > 0 {
		s.log.Printf("serve: stopping; waiting for %d running turn(s) to finish", n)
	}
	for {
		s.mu.Lock()
		n = len(s.turns)
		s.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-stop:
			s.mu.Lock()
			for _, cancel := range s.turns {
				cancel()
			}
			s.mu.Unlock()
			s.log.Printf("serve: stopping now; %d turn(s) stopped where they were", n)
			for {
				s.mu.Lock()
				n = len(s.turns)
				s.mu.Unlock()
				if n == 0 {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func key(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

func branch(n int) string { return fmt.Sprintf("harness/%d", n) }

func (s *Service) clone(p *proj) string {
	return filepath.Join(s.cfg.StateDir, "repos", p.cfg.Repo+".git")
}

func (s *Service) worktree(p *proj, n int) string {
	return filepath.Join(s.cfg.StateDir, "worktrees", p.cfg.Repo, fmt.Sprint(n))
}

// shared are the folders of the state directory the bots' user works in:
// their worktrees, the clones those belong to, the owner's attachments, and
// their agents' configuration and sessions. The database and the logs stay the
// service's own.
var shared = []string{"worktrees", "repos", "attachments", "agents"}

// prepareStateDir makes the state directory, and, with a bots' user, gives
// the shared folders to the group: setgid, so what is made in them is the
// group's too. The service runs with a umask that leaves the group
// read-write (see the unit harness deploy writes).
func (s *Service) prepareStateDir() error {
	if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
		return err
	}
	if s.cfg.Group == "" {
		return nil
	}
	g, err := user.LookupGroup(s.cfg.Group)
	if err != nil {
		return fmt.Errorf("opencode.group: %w", err)
	}
	if s.gid, err = strconv.Atoi(g.Gid); err != nil {
		return err
	}
	// The bots' user reaches its folders through the state directory, which
	// it may enter but not list.
	if err := s.share(s.cfg.StateDir, 0o710); err != nil {
		return err
	}
	for _, d := range shared {
		dir := filepath.Join(s.cfg.StateDir, d)
		if err := os.MkdirAll(dir, 0o770); err != nil {
			return err
		}
		if err := s.share(dir, 0o770); err != nil {
			return err
		}
	}
	return nil
}

// share gives a folder to the bots' group, setgid, unless it is already
// theirs. A folder the host made right is left alone: chmod by a user
// outside the group would clear the setgid bit.
func (s *Service) share(dir string, perm os.FileMode) error {
	want := perm | os.ModeSetgid
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Gid) == s.gid && info.Mode()&(os.ModePerm|os.ModeSetgid) == want {
		return nil
	}
	if err := os.Chown(dir, -1, s.gid); err != nil {
		return fmt.Errorf("give %s to group %s: %w; this user must be in the group", dir, s.cfg.Group, err)
	}
	return os.Chmod(dir, want)
}

// dirMode and fileMode are for what the bots' user reads or writes: the
// group's too when there is one.
func (s *Service) dirMode() os.FileMode {
	if s.gid >= 0 {
		return 0o770
	}
	return 0o700
}

func (s *Service) fileMode() os.FileMode {
	if s.gid >= 0 {
		return 0o640
	}
	return 0o600
}

// git runs in the project's clone, or in a worktree of it.
func (s *Service) git(p *proj, dir string) (git.Git, error) {
	tok, err := s.gh.Token(p.cfg.Repo)
	if err != nil {
		return git.Git{}, err
	}
	if dir == "" {
		dir = s.clone(p)
	}
	return git.Git{Dir: dir, Token: tok, Name: s.cfg.Author.Name, Email: s.cfg.Author.Email}, nil
}

// fetch brings the clone up to date with GitHub, cloning it the first time.
// The clone is bare: nothing works in it but the worktrees.
func (s *Service) fetch(p *proj) error {
	g, err := s.git(p, "")
	if err != nil {
		return err
	}
	if _, err := os.Stat(g.Dir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(g.Dir), s.dirMode()); err != nil {
			return err
		}
		parent := g
		parent.Dir = filepath.Dir(g.Dir)
		if _, err := parent.Run("clone", "--bare", "--quiet", p.cfg.GitURL, g.Dir); err != nil {
			return err
		}
		// A bare clone maps the remote's branches onto its own. Keep them as
		// origin/*, as a normal clone does, so the worktrees' branches are
		// never overwritten by a fetch.
		if _, err := g.Run("config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return err
		}
	}
	_, err = g.Run("fetch", "--quiet", "--prune", "origin")
	return err
}

// poll asks GitHub what changed in one project.
func (s *Service) poll(ctx context.Context, p *proj) error {
	base, err := s.gh.DefaultBranch(p.cfg.Repo)
	if err != nil {
		return err
	}
	s.mu.Lock()
	p.base = base
	s.mu.Unlock()
	if err := s.fetch(p); err != nil {
		return err
	}
	if err := s.syncSettings(p); err != nil {
		return err
	}
	if p.repo == nil {
		return nil
	}

	// Only what happens after harness serve starts watching a project is work:
	// the issues already open then are left alone.
	startKey := "start:" + p.cfg.Repo
	startText, err := s.st.Get(startKey)
	if err != nil {
		return err
	}
	now := time.Now()
	if startText == "" {
		startText = now.UTC().Format(time.RFC3339)
		if err := s.st.Set(startKey, startText); err != nil {
			return err
		}
	}
	start, _ := time.Parse(time.RFC3339, startText)

	// New threads: issues updated since the last poll, with a margin for
	// GitHub's lists trailing a change by a few seconds.
	pollKey := "poll:" + p.cfg.Repo
	since := start
	if last, _ := s.st.Get(pollKey); last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			since = t.Add(-5 * time.Minute)
		}
	}
	issues, err := s.gh.Issues(p.cfg.Repo, since)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.State != "open" {
			continue
		}
		t, err := s.st.Thread(p.cfg.Repo, issue.Number)
		if err != nil {
			return err
		}
		if t == nil || t.State == store.Closed {
			if err := s.refresh(p, &store.Thread{Repo: p.cfg.Repo, Number: issue.Number, State: store.Idle}, start, true); err != nil {
				s.log.Printf("%s: %v", key(p.cfg.Repo, issue.Number), err)
			}
		}
	}
	if err := s.st.Set(pollKey, now.UTC().Format(time.RFC3339)); err != nil {
		return err
	}

	threads, err := s.st.Threads()
	if err != nil {
		return err
	}
	for _, t := range threads {
		if t.Repo == p.cfg.Repo {
			if err := s.refresh(p, t, start, false); err != nil {
				s.log.Printf("%s: %v", key(t.Repo, t.Number), err)
			}
		}
	}
	return nil
}

// refresh works out a thread's unanswered events from GitHub, and closes it
// when its issue or pull request has closed. A thread that is new is saved
// only if it has something to answer.
func (s *Service) refresh(p *proj, t *store.Thread, start time.Time, isNew bool) error {
	issue, err := s.gh.Issue(t.Repo, t.Number)
	if err != nil {
		return err
	}
	if issue.State != "open" {
		if !isNew {
			return s.close(p, t.Number, "the issue was closed")
		}
		return nil
	}
	// The thread's own pull request closing closes the thread. A closed one
	// found by its branch is from before the issue was reopened.
	var pr *github.PR
	if t.PR != 0 {
		if pr, err = s.gh.PR(t.Repo, t.PR); err != nil {
			return err
		}
		if pr.State != "open" {
			why := "its pull request was closed"
			if pr.MergedAt != nil {
				why = "its pull request was merged"
			}
			return s.close(p, t.Number, why)
		}
		// Between turns, the pull request is what the bot last published.
		// During one, the bot may be publishing.
		s.mu.Lock()
		running := s.turns[key(t.Repo, t.Number)] != nil
		s.mu.Unlock()
		if !running {
			s.restorePR(t, pr.Title, pr.Body)
		}
	} else {
		if pr, err = s.gh.FindPR(t.Repo, branch(t.Number)); err != nil {
			return err
		}
		if pr != nil && pr.State != "open" {
			pr = nil
		}
	}

	events, err := s.events(p, t, issue, pr, start)
	if err != nil {
		return err
	}
	agent, scope, classErr := s.classify(p, t.Number)
	handled, err := s.st.Handled(t.Repo, t.Number)
	if err != nil {
		return err
	}
	var inbox []store.Event
	for _, e := range events {
		if !handled[e.Key] {
			inbox = append(inbox, e)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// An issue with no scope label is no bot's: it is left alone, and
	// found again when a label is added, which updates it.
	unscoped := classErr == nil && scope == ""
	if isNew && (len(inbox) == 0 || unscoped) {
		return nil
	}
	p.inbox[t.Number] = inbox
	cur, err := s.st.Thread(t.Repo, t.Number)
	if err != nil {
		return err
	}
	switch {
	case cur == nil:
		cur = &store.Thread{Repo: t.Repo, Number: t.Number, State: store.Idle}
		s.log.Printf("%s: new thread", key(t.Repo, t.Number))
	case cur.State == store.Closed:
		// The same session carries on, with what it knew; its pull request
		// is gone.
		cur.State, cur.PR, cur.Nudged = store.Idle, 0, false
		s.log.Printf("%s: reopened", key(t.Repo, t.Number))
	}
	if pr != nil {
		cur.PR = pr.Number
	}
	switch {
	case classErr != nil:
		cur.Agent = ""
		s.labelProblem(p, t.Number, classErr)
	case unscoped:
		// Its scope label was taken off: no more turns until one is back.
		cur.Agent = ""
	case cur.State != store.Running:
		// A new agent starts a new session: sessions are the agent's own.
		if cur.Agent != agent {
			cur.Session = ""
		}
		cur.Agent, cur.Scope = agent, scope
	}
	// Something new since the bot was blocked: give it another go.
	if cur.State == store.Blocked && len(inbox) > 0 {
		cur.State = store.Idle
	}
	return s.st.Save(cur)
}

// close ends a thread: its turn is stopped, and its worktree and local
// branch removed. Its agent's session stays.
func (s *Service) close(p *proj, n int, why string) error {
	s.mu.Lock()
	cancel := s.turns[key(p.cfg.Repo, n)]
	delete(p.inbox, n)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := s.update(p.cfg.Repo, n, func(t *store.Thread) error {
		t.State, t.Batch, t.Outcome = store.Closed, nil, ""
		return nil
	}); err != nil {
		return err
	}
	g, err := s.git(p, "")
	if err == nil {
		g.Run("worktree", "remove", "--force", s.worktree(p, n))
		g.Run("worktree", "prune")
		g.Run("branch", "-D", branch(n))
	}
	s.log.Printf("%s: closed: %s", key(p.cfg.Repo, n), why)
	return nil
}

// update changes a saved thread under the lock, so a poll and a bot's
// command never overwrite each other's change.
func (s *Service) update(repo string, n int, change func(t *store.Thread) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.st.Thread(repo, n)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("%s: no such thread", key(repo, n))
	}
	if err := change(t); err != nil {
		return err
	}
	return s.st.Save(t)
}

// pending is a thread's unanswered events, less those already answered,
// which a poll may not have caught up with yet.
func (s *Service) pending(p *proj, n int) ([]store.Event, error) {
	handled, err := s.st.Handled(p.cfg.Repo, n)
	if err != nil {
		return nil, err
	}
	var out []store.Event
	for _, e := range p.inbox[n] {
		if !handled[e.Key] {
			out = append(out, e)
		}
	}
	var kept []retry
	for _, r := range s.retries[n] {
		if r.repo != p.cfg.Repo {
			kept = append(kept, r)
		} else if !handled[r.event.Key] {
			out = append(out, r.event)
			kept = append(kept, r)
		}
	}
	s.retries[n] = kept
	return out, nil
}

// schedule starts a turn for each idle thread with something to answer, the
// one waiting longest first, while there are free slots.
func (s *Service) schedule(ctx context.Context) {
	type ready struct {
		p     *proj
		t     *store.Thread
		inbox []store.Event
	}
	var queue []ready
	s.mu.Lock()
	threads, err := s.st.Threads()
	if err != nil {
		s.mu.Unlock()
		s.log.Printf("schedule: %v", err)
		return
	}
	for _, t := range threads {
		p := s.project(t.Repo)
		if p == nil || t.State != store.Idle || s.turns[key(t.Repo, t.Number)] != nil {
			continue
		}
		// Another server's agent: that server works it.
		if t.Agent == "" || !s.cfg.Runs(t.Agent) {
			continue
		}
		inbox, err := s.pending(p, t.Number)
		if err != nil {
			s.log.Printf("schedule: %v", err)
			continue
		}
		if len(inbox) > 0 {
			queue = append(queue, ready{p, t, inbox})
		}
	}
	s.mu.Unlock()
	slices.SortFunc(queue, func(a, b ready) int { return a.inbox[0].At.Compare(b.inbox[0].At) })
	for _, r := range queue {
		select {
		case s.slots <- struct{}{}:
			s.start(ctx, r.p, r.t, r.inbox)
		default:
			return
		}
	}
}

func (s *Service) project(repo string) *proj {
	for _, p := range s.projects {
		if p.cfg.Repo == repo {
			return p
		}
	}
	return nil
}

// threadFor finds the running thread whose worktree a bot's command ran in.
func (s *Service) threadFor(cwd string) (*proj, *store.Thread, error) {
	dir, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, nil, err
	}
	threads, err := s.st.Threads()
	if err != nil {
		return nil, nil, err
	}
	for _, t := range threads {
		p := s.project(t.Repo)
		if p == nil {
			continue
		}
		wt, err := filepath.EvalSymlinks(s.worktree(p, t.Number))
		if err != nil {
			continue
		}
		if dir == wt || strings.HasPrefix(dir, wt+string(filepath.Separator)) {
			return p, t, nil
		}
	}
	return nil, nil, fmt.Errorf("%s is not the worktree of a bot thread; run harness from inside your worktree", cwd)
}
