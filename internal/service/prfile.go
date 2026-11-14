package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/michaelckearney/harness/internal/gitversion"
	"github.com/michaelckearney/harness/internal/store"
)

// prFile is the bot's pull request, in its worktree: the first line is the
// title, the rest the description. Each harness commit publishes it, and to
// the bot it is the only account of the pull request: what anyone else
// writes there on GitHub is put back.
const prFile = ".harness/pr.md"

// prTemplate is what a new worktree's prFile starts as. It has no title,
// so the first harness commit asks for one.
const prTemplate = `<!--
This file is your pull request. Each harness commit publishes it as the
pull request's title and description.

The first line after this comment is the title, a conventional commit:
  feat: <what a user can now do>
  fix: <what no longer goes wrong>
  feat!: or fix!: for a change that breaks something that worked before
  docs:, test:, refactor:, chore:, ci:, or build: for anything else
The lines after the title are the description, in Markdown: what changed,
why, and how you verified it. An image, ![what it shows](path), is
uploaded; a relative path starts from the worktree's root.

Keep it current as your changes grow. Git ignores this file.
-->
`

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// prText is prFile read: its title, its description, and the sum of the
// file, which says whether it changed since it was last published.
type prText struct {
	Title, Body, Sum string
}

// prSum is the sum of the worktree's prFile, "" when there is none.
func (s *Service) prSum(p *proj, n int) string {
	data, err := os.ReadFile(filepath.Join(s.worktree(p, n), prFile))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// prShown is prFile without its comments, for status.
func (s *Service) prShown(p *proj, n int) string {
	data, _ := os.ReadFile(filepath.Join(s.worktree(p, n), prFile))
	return strings.TrimSpace(htmlComment.ReplaceAllString(string(data), ""))
}

// readPR reads and checks the worktree's prFile, the same way for every
// commit.
func (s *Service) readPR(p *proj, n int) (prText, error) {
	wt := s.worktree(p, n)
	data, err := os.ReadFile(filepath.Join(wt, prFile))
	if errors.Is(err, os.ErrNotExist) {
		return prText{}, fixable(fmt.Sprintf("write your pull request's title and description in %s first: the first line is the title, the rest the description", prFile))
	}
	if err != nil {
		return prText{}, err
	}
	text := strings.TrimSpace(htmlComment.ReplaceAllString(string(data), ""))
	title, rest, _ := strings.Cut(text, "\n")
	title = strings.TrimSpace(title)
	body := strings.TrimSpace(rest)
	switch {
	case title == "":
		return prText{}, fixable(fmt.Sprintf("write your pull request's title and description in %s first: the first line after its comment is the title, the rest the description", prFile))
	case !gitversion.Conventional(title):
		return prText{}, fixable(fmt.Sprintf("the title in %s, %q, is not a conventional commit, such as \"feat: add an export button\", \"fix: keep the window size\", or \"docs: explain modes\". feat! marks a breaking change", prFile, title))
	case body == "":
		return prText{}, fixable(fmt.Sprintf("after the title in %s, describe the changes: what changed, why, and how you verified it", prFile))
	}
	// Images that cannot be shown are refused now, before anything is pushed.
	if _, _, err := attachments(body, wt, wt); err != nil {
		return prText{}, err
	}
	return prText{Title: title, Body: body, Sum: s.prSum(p, n)}, nil
}

// setupPRFile has git ignore prFile in every worktree of the clone, and
// gives the worktree the template when it has no prFile.
func (s *Service) setupPRFile(p *proj, n int) error {
	g, err := s.git(p, "")
	if err != nil {
		return err
	}
	common, err := g.Run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	exclude := filepath.Join(common, "info", "exclude")
	line := "/" + filepath.Dir(prFile) + "/"
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !strings.Contains("\n"+string(data), "\n"+line+"\n") {
		if err := os.MkdirAll(filepath.Dir(exclude), s.dirMode()); err != nil {
			return err
		}
		if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
			data = append(data, '\n')
		}
		if err := os.WriteFile(exclude, append(data, line+"\n"...), 0o644); err != nil {
			return err
		}
	}
	path := filepath.Join(s.worktree(p, n), prFile)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, s.dirMode()); err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, []byte(prTemplate), 0o600); err != nil {
			return err
		}
	}
	// The bot writes it: when the bots have their own user, the folder and
	// the file are their group's, whatever group the service makes files
	// with. Done each time, so a worktree made before is put right too.
	mode := os.FileMode(0o600)
	if s.gid >= 0 {
		mode = 0o660
	}
	if err := s.give(dir, s.dirMode()); err != nil {
		return err
	}
	return s.give(path, mode)
}

// give makes f the bots' group's, with mode, if it is not already. Claude
// Code's edits replace the file, so the bots' user owns it then, and only
// its owner may change it: changing it anyway failed every turn, and the
// thread went round and round without a word (monorepo#17).
func (s *Service) give(f string, mode os.FileMode) error {
	info, err := os.Stat(f)
	if err != nil {
		return err
	}
	if s.gid >= 0 && fileGid(info) != s.gid {
		if err := os.Chown(f, -1, s.gid); err != nil {
			return fmt.Errorf("give %s to the bots' group: %w", f, err)
		}
	}
	if info.Mode().Perm() != mode {
		return os.Chmod(f, mode)
	}
	return nil
}

// publish makes the pull request what prFile says, opening it if there is
// none, and keeps what GitHub then holds, so an edit by anyone else can be
// put back.
func (s *Service) publish(p *proj, t *store.Thread, pt prText) (string, error) {
	if t.PR != 0 && pt.Sum == t.PRSource {
		return fmt.Sprintf("Pull request #%d is updated.", t.PR), nil
	}
	wt := s.worktree(p, t.Number)
	body := prBody(pt.Body, t.Number)
	_, files, err := attachments(body, wt, wt)
	if err != nil {
		return "", err
	}
	n, out, opened := t.PR, "", false
	if n == 0 {
		opened = true
		s.mu.Lock()
		base := p.base
		s.mu.Unlock()
		pr, err := s.gh.CreatePR(t.Repo, pt.Title, body, branch(t.Number), base, true)
		if err != nil {
			return "", err
		}
		n = pr.Number
		t.PR = n
		if err := s.update(t.Repo, t.Number, func(t *store.Thread) error { t.PR = n; return nil }); err != nil {
			return "", err
		}
		out = fmt.Sprintf("Opened draft pull request #%d from %s: %s", n, prFile, pr.HTMLURL)
	} else {
		if err := s.gh.SetTitle(t.Repo, n, pt.Title); err != nil {
			return "", err
		}
		out = fmt.Sprintf("Pull request #%d is updated, with its title and description from %s.", n, prFile)
	}
	// The description goes up through post, which uploads its images. A
	// pull request just opened has it already, unless it shows images.
	if !opened || len(files) > 0 {
		if err := s.post(p, t, true, n, body, wt); err != nil {
			return "", err
		}
	}
	pr, err := s.gh.PR(t.Repo, n)
	if err != nil {
		return "", err
	}
	if err := s.update(t.Repo, t.Number, func(t *store.Thread) error {
		t.PRTitle, t.PRBody, t.PRSource = pr.Title, pr.Body, pt.Sum
		return nil
	}); err != nil {
		return "", err
	}
	t.PRTitle, t.PRBody, t.PRSource = pr.Title, pr.Body, pt.Sum
	return out, nil
}

// restorePR puts back the title and description the harness last
// published, when someone else changed them. The bot is not told: to it,
// prFile is the pull request.
func (s *Service) restorePR(t *store.Thread, title, body string) {
	if t.PRTitle == "" {
		return
	}
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	if norm(title) != norm(t.PRTitle) {
		if err := s.gh.SetTitle(t.Repo, t.PR, t.PRTitle); err != nil {
			s.log.Printf("%s: putting back the title: %v", key(t.Repo, t.Number), err)
		} else {
			s.log.Printf("%s: put back the pull request's title", key(t.Repo, t.Number))
		}
	}
	if norm(body) != norm(t.PRBody) {
		if err := s.gh.SetBody(t.Repo, t.PR, t.PRBody); err != nil {
			s.log.Printf("%s: putting back the description: %v", key(t.Repo, t.Number), err)
		} else {
			s.log.Printf("%s: put back the pull request's description", key(t.Repo, t.Number))
		}
	}
}

// fileGid is the group that owns a file, or -1 where the system does not
// say.
func fileGid(info os.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Gid)
	}
	return -1
}
