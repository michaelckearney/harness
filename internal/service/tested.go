package service

import (
	"fmt"
	"os"
	"path/filepath"

	gitpkg "github.com/michaelckearney/harness/internal/git"
	"github.com/michaelckearney/harness/internal/repo"
	"github.com/michaelckearney/harness/internal/store"
)

// The whole of harness test, run during a turn, reports to the service: it
// asks for the tree it is about to test, and says afterwards whether that
// tree passed. done then knows whether what the turn committed was tested.
//
// The tree is the worktree's files, committed or not, as git would commit
// them, so testing before committing counts. The service works it out,
// not the bot's command: the bots' user cannot write the clone's objects.

// testBegin returns the tree of the worktree's files as they are now, for a
// full test of the scope args names, which must cover the thread's.
func (s *Service) testBegin(p *proj, t *store.Thread, args []string) (string, error) {
	s.mu.Lock()
	cfg := p.repo
	s.mu.Unlock()
	if cfg == nil {
		return "", fmt.Errorf("the default branch has no valid %s", repo.File)
	}
	tested := cfg.FrameworkScope()
	if len(args) == 1 && args[0] != "" {
		tested = s.scopeOf(p, args[0])
	}
	own := s.scopeOf(p, t.Scope)
	if !cfg.Covers(tested, own) {
		return "", fixable(fmt.Sprintf("a test of %s does not cover this issue's %s; run harness test with no project or framework named", tested, own))
	}
	g, err := s.wgit(p, t)
	if err != nil {
		return "", err
	}
	return workTree(g)
}

// tested records that the whole suite ran on a tree, and whether it passed.
func (s *Service) tested(p *proj, t *store.Thread, args []string) (string, error) {
	if len(args) != 2 || (args[1] != "pass" && args[1] != "fail") {
		return "", fixable("tested takes a tree and pass or fail")
	}
	tree, pass := args[0], args[1] == "pass"
	if err := s.update(t.Repo, t.Number, func(t *store.Thread) error {
		t.TestedTree, t.TestedPass = tree, pass
		return nil
	}); err != nil {
		return "", err
	}
	return "", nil
}

// workTree is the tree git would commit from the worktree, made with an
// index of its own, so the worktree's own index is untouched.
func workTree(g gitpkg.Git) (string, error) {
	dir, err := os.MkdirTemp("", "harness-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	g.Env = append(g.Env, "GIT_INDEX_FILE="+filepath.Join(dir, "index"))
	if _, err := g.Run("read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := g.Run("add", "-A"); err != nil {
		return "", err
	}
	return g.Run("write-tree")
}

// committedThisTurn reports whether the running turn made commits of its
// own: merging the default branch in does not count.
func (s *Service) committedThisTurn(p *proj, g gitpkg.Git, t *store.Thread) bool {
	if t.TurnHead == "" {
		return false
	}
	s.mu.Lock()
	base := p.base
	s.mu.Unlock()
	out, err := g.Run("rev-list", "--no-merges", t.TurnHead+"..HEAD", "--not", "refs/remotes/origin/"+base)
	return err == nil && out != ""
}

// humanAsked reports whether a person is behind any of the turn's events.
// CI failing and the default branch moving on need work, not a reply.
func humanAsked(batch []store.Event) bool {
	for _, e := range batch {
		if e.Kind != "ci-failed" && e.Kind != "base-moved" {
			return true
		}
	}
	return false
}
