package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	gitpkg "github.com/michaelckearney/harness/internal/git"
	"github.com/michaelckearney/harness/internal/github"
	"github.com/michaelckearney/harness/internal/repo"
	"github.com/michaelckearney/harness/internal/rpc"
	"github.com/michaelckearney/harness/internal/store"
)

// fixable is an error the bot can do something about. Its message says what.
type fixable string

func (f fixable) Error() string { return string(f) }

// listen takes the socket, unless another service is answering on it.
//
// The socket's file is removed on the way out only if it is still this
// service's: Go's listener would remove whatever is at the path. The
// botswain service, stopped after harness serve had started in its place,
// took the new socket with it, and the bots' commands failed for the next
// hour. See also keepSocket.
func (s *Service) listen() (*net.UnixListener, error) {
	path := s.cfg.Socket()
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("another service is answering on %s; stop it first", path)
	}
	os.Remove(path)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(false)
	// Only this user, and the bots' group when they run as a user of their
	// own: nobody else should drive them.
	mode := os.FileMode(0o600)
	if s.gid >= 0 {
		// The state directory is setgid, so the socket is the group's
		// already; chown makes sure.
		mode = 0o660
		if err := os.Chown(path, -1, s.gid); err != nil {
			ln.Close()
			return nil, fmt.Errorf("give %s to group %s: %w", path, s.cfg.Group, err)
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		ln.Close()
		return nil, err
	}
	s.mu.Lock()
	s.socket = info
	s.mu.Unlock()
	return ln, nil
}

// unlisten closes the socket, and removes its file if it is still this
// service's.
func (s *Service) unlisten(ln net.Listener) {
	ln.Close()
	s.mu.Lock()
	mine := s.socket
	s.mu.Unlock()
	if info, err := os.Stat(s.cfg.Socket()); err == nil && mine != nil && os.SameFile(info, mine) {
		os.Remove(s.cfg.Socket())
	}
}

// keepSocket listens again if the socket's file is gone or is no longer
// this service's, so whatever removed it costs the bots a poll, not the
// rest of the day.
func (s *Service) keepSocket(ln *net.UnixListener) *net.UnixListener {
	s.mu.Lock()
	mine := s.socket
	s.mu.Unlock()
	if info, err := os.Stat(s.cfg.Socket()); err == nil && mine != nil && os.SameFile(info, mine) {
		return ln
	}
	s.log.Printf("serve: %s was removed; listening again", s.cfg.Socket())
	if ln != nil {
		ln.Close()
	}
	next, err := s.listen()
	if err != nil {
		s.log.Printf("serve: %v", err)
		return ln
	}
	go s.serve(next)
	return next
}

func (s *Service) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			var req rpc.Request
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				return
			}
			json.NewEncoder(conn).Encode(s.handle(req))
		}()
	}
}

func (s *Service) handle(req rpc.Request) rpc.Response {
	if req.Cmd == "queue" {
		out, err := s.queue()
		return respond(out, err)
	}
	s.mu.Lock()
	p, t, err := s.threadFor(req.Cwd)
	s.mu.Unlock()
	if err != nil {
		return respond("", fixable(err.Error()))
	}
	if t.State != store.Running {
		return respond("", fixable(fmt.Sprintf("issue #%d has no turn running; the harness only acts during one", t.Number)))
	}
	if t.Outcome != "" && req.Cmd != "status" && req.Cmd != "diff" && req.Cmd != "scope" {
		return respond("", fixable("this turn has already ended; stop here"))
	}
	var out string
	switch req.Cmd {
	case "status":
		out, err = s.status(p, t)
	case "diff":
		out, err = s.diff(p, t)
	case "commit":
		out, err = s.commit(p, t, strings.Join(req.Args, " "))
	case "sync":
		out, err = s.sync(p, t, "")
	case "scope":
		out, err = s.scopeJSON(p, t)
	case "test-begin":
		out, err = s.testBegin(p, t, req.Args)
	case "tested":
		out, err = s.tested(p, t, req.Args)
	case "reply":
		out, err = s.reply(p, t, req.Body, req.BodyDir)
	case "done":
		out, err = s.done(p, t)
	default:
		err = fixable(fmt.Sprintf("%q is not a bot command; run harness help", req.Cmd))
	}
	return respond(out, err)
}

func respond(out string, err error) rpc.Response {
	var f fixable
	switch {
	case err == nil:
		return rpc.Response{Output: out}
	case errors.As(err, &f):
		return rpc.Response{Output: f.Error(), Code: 1}
	default:
		return rpc.Response{Output: "harness serve failed: " + err.Error(), Code: 2}
	}
}

func (s *Service) wgit(p *proj, t *store.Thread) (gitpkg.Git, error) {
	return s.git(p, s.worktree(p, t.Number))
}

// status is everything the bot needs to know about its thread: this turn's
// events first, then the whole conversation.
func (s *Service) status(p *proj, t *store.Thread) (string, error) {
	repo, n := t.Repo, t.Number
	issue, err := s.gh.Issue(repo, n)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Issue #%d: %s\n%s\n\n", n, issue.Title, issue.HTMLURL)
	fmt.Fprintf(&b, "Branch: %s\n", branch(n))
	g, err := s.wgit(p, t)
	if err != nil {
		return "", err
	}
	if changes, _ := g.Run("status", "--porcelain"); changes != "" {
		fmt.Fprintf(&b, "Uncommitted changes: yes, see harness diff\n")
	}
	var pr *github.PR
	var ciLogs string
	if t.PR != 0 {
		if pr, err = s.gh.PR(repo, t.PR); err != nil {
			return "", err
		}
		state := "ready for review"
		if pr.Draft {
			state = "draft"
		}
		fmt.Fprintf(&b, "Pull request: #%d (%s) %s\n", pr.Number, state, pr.HTMLURL)
		if s.prSum(p, n) != t.PRSource {
			fmt.Fprintf(&b, "%s: changed since your last harness commit, which publishes it\n", prFile)
		}
		runs, err := s.gh.CheckRuns(repo, pr.Head.SHA)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "CI on %s: %s\n", short(pr.Head.SHA), ciState(runs))
		ciLogs = s.ciLogs(repo, runs)
	} else {
		fmt.Fprintf(&b, "Pull request: none yet; your first harness commit opens one from %s\n", prFile)
	}

	fmt.Fprintf(&b, "\n## New this turn\n\n")
	for _, e := range t.Batch {
		fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(strings.TrimSpace(e.Text), "\n", "\n  "))
	}
	b.WriteString(ciLogs)

	if shown := s.prShown(p, n); shown != "" {
		fmt.Fprintf(&b, "\n## Your pull request, in %s\n\n%s\n", prFile, shown)
	} else {
		fmt.Fprintf(&b, "\n## Your pull request, in %s\n\nNot written yet. Write its title and description there before your first harness commit.\n", prFile)
	}

	fmt.Fprintf(&b, "\n## The whole conversation\n\n")
	type entry struct {
		at   time.Time
		text string
	}
	entries := []entry{{issue.CreatedAt, fmt.Sprintf("### @%s opened the issue\n\n%s", issue.User.Login, s.attachedFiles(p, n, issue.Body, issue.BodyHTML))}}
	comments, err := s.gh.Comments(repo, n)
	if err != nil {
		return "", err
	}
	if pr != nil {
		more, err := s.gh.Comments(repo, pr.Number)
		if err != nil {
			return "", err
		}
		comments = append(comments, more...)
	}
	for _, c := range comments {
		entries = append(entries, entry{c.CreatedAt, fmt.Sprintf("### @%s\n\n%s", c.User.Login, s.attachedFiles(p, n, c.Body, c.BodyHTML))})
	}
	if pr != nil {
		reviews, err := s.gh.Reviews(repo, pr.Number)
		if err != nil {
			return "", err
		}
		rcs, err := s.gh.ReviewComments(repo, pr.Number)
		if err != nil {
			return "", err
		}
		for _, r := range reviews {
			if r.State == "PENDING" {
				continue
			}
			text := fmt.Sprintf("### @%s reviewed: %s\n\n%s", r.User.Login, strings.ToLower(strings.ReplaceAll(r.State, "_", " ")), s.attachedFiles(p, n, r.Body, r.BodyHTML))
			for _, c := range rcs {
				if c.ReviewID == r.ID {
					text += fmt.Sprintf("\n\n%s: %s", where(c), s.attachedFiles(p, n, c.Body, c.BodyHTML))
				}
			}
			entries = append(entries, entry{r.SubmittedAt, text})
		}
	}
	slices.SortStableFunc(entries, func(a, b entry) int { return a.at.Compare(b.at) })
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(e.text))
	}
	fmt.Fprintf(&b, "Answer with harness reply, then end this turn with harness done.\n")
	return b.String(), nil
}

// ciLogs is, for each check that failed, the end of its job's log: where
// it failed, and every failed Go test. The bot has no other way to see it.
func (s *Service) ciLogs(repo string, runs []github.CheckRun) string {
	var b strings.Builder
	for _, r := range runs {
		if r.Status != "completed" || r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\n## Why CI failed\n")
		}
		fmt.Fprintf(&b, "\n### %s: %s\n\n", r.Name, r.Conclusion)
		log, err := s.gh.JobLog(repo, r.ID)
		var ge *github.Error
		switch {
		case errors.As(err, &ge) && (ge.Status == 403 || ge.Status == 404):
			b.WriteString("Its log cannot be read: the GitHub App needs read access to Actions, which a person gives it in the App's settings.\n")
		case err != nil:
			fmt.Fprintf(&b, "Its log cannot be read: %v\n", err)
		default:
			fmt.Fprintf(&b, "The end of its log:\n\n```\n%s\n```\n", logExcerpt(log))
		}
	}
	return b.String()
}

var (
	logStamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z `)
	logColor = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

// logExcerpt is what a job's log says about its failure: every line a Go
// test failed on, then the lines before its first error, without
// timestamps and colours, and no more than about a hundred lines.
func logExcerpt(log string) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(log, "\r", ""), "\n") {
		lines = append(lines, logColor.ReplaceAllString(logStamp.ReplaceAllString(l, ""), ""))
	}
	end := len(lines)
	for i, l := range lines {
		if strings.Contains(l, "##[error]") {
			end = i + 1
			break
		}
	}
	var out []string
	for _, l := range lines[:end] {
		if strings.Contains(l, "--- FAIL") && len(out) < 30 {
			out = append(out, l)
		}
	}
	if len(out) > 0 {
		out = append(out, "...")
	}
	out = append(out, lines[max(0, end-70):end]...)
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func ciState(runs []github.CheckRun) string {
	if len(runs) == 0 {
		return "no checks"
	}
	var bad, waiting []string
	for _, r := range runs {
		switch {
		case r.Status != "completed":
			waiting = append(waiting, r.Name)
		case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
		default:
			bad = append(bad, fmt.Sprintf("%s (%s)", r.Name, r.Conclusion))
		}
	}
	switch {
	case len(bad) > 0:
		return "failing: " + strings.Join(bad, ", ")
	case len(waiting) > 0:
		return "running: " + strings.Join(waiting, ", ")
	}
	return "passing"
}

func (s *Service) diff(p *proj, t *store.Thread) (string, error) {
	g, err := s.wgit(p, t)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	base := p.base
	s.mu.Unlock()
	mb, err := g.Run("merge-base", "HEAD", "refs/remotes/origin/"+base)
	if err != nil {
		return "", err
	}
	diff, err := g.Run("diff", mb)
	if err != nil {
		return "", err
	}
	untracked, err := g.Run("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	if diff == "" && untracked == "" {
		return fmt.Sprintf("No changes against %s.", base), nil
	}
	if untracked != "" {
		diff += "\n\nNew files, not shown above:\n" + indent(untracked)
	}
	return diff, nil
}

func clean(g gitpkg.Git) error {
	if _, err := g.Run("rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		return fixable("a merge is in progress; fix its conflicts, then run harness commit \"<message>\"")
	}
	if changes, _ := g.Run("status", "--porcelain"); changes != "" {
		return fixable("you have uncommitted changes; run harness commit \"<message>\" first, or undo them")
	}
	return nil
}

// commit commits everything in the worktree and pushes it, then makes the
// pull request what the bot's pull request file says, the same way every
// time: the first push opens it as a draft.
func (s *Service) commit(p *proj, t *store.Thread, msg string) (string, error) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "", fixable("say what the commit does: harness commit \"Fix the date in the header\"")
	}
	pt, err := s.readPR(p, t.Number)
	if err != nil {
		return "", err
	}
	g, err := s.wgit(p, t)
	if err != nil {
		return "", err
	}
	if _, err := g.Run("add", "-A"); err != nil {
		return "", err
	}
	if files, _ := g.Run("diff", "--name-only", "--diff-filter=U"); files != "" {
		return "", fixable("these files still have merge conflicts; fix them, then commit again:\n" + indent(files))
	}
	staged, err := g.Run("diff", "--cached", "-U0")
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(staged, "\n") {
		if strings.HasPrefix(l, "+<<<<<<< ") || strings.HasPrefix(l, "+>>>>>>> ") {
			return "", fixable("your changes still have conflict markers (<<<<<<< or >>>>>>>); remove them, then commit again")
		}
	}
	_, mergeErr := g.Run("rev-parse", "-q", "--verify", "MERGE_HEAD")
	if staged == "" && mergeErr != nil {
		// Only the pull request file changed: publish it.
		if t.PR != 0 && pt.Sum != t.PRSource {
			out, err := s.publish(p, t, pt)
			if err != nil {
				return "", err
			}
			return "There was nothing to commit. " + out, nil
		}
		return "", fixable("there is nothing to commit")
	}
	if err := s.inScope(p, t, g, mergeErr == nil); err != nil {
		g.Run("reset", "--quiet")
		return "", err
	}
	if _, err := g.Run("commit", "--quiet", "-m", msg); err != nil {
		return "", err
	}
	sha, _ := g.Run("rev-parse", "--short", "HEAD")
	out, err := s.push(p, t, g, pt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Committed %s and pushed it to %s.\n%s", sha, branch(t.Number), out), nil
}

// push sends the branch to GitHub, never forced, then publishes pt, the
// bot's pull request file, opening the draft pull request if there is none.
func (s *Service) push(p *proj, t *store.Thread, g gitpkg.Git, pt prText) (string, error) {
	if _, err := g.Run("push", "--quiet", "origin", "HEAD:refs/heads/"+branch(t.Number)); err != nil {
		return "", fixable("GitHub refused the push: " + err.Error())
	}
	return s.publish(p, t, pt)
}

// sync merges the default branch into the thread's branch: where it is on
// GitHub now, or, given a commit of it, that commit.
func (s *Service) sync(p *proj, t *store.Thread, commit string) (string, error) {
	g, err := s.wgit(p, t)
	if err != nil {
		return "", err
	}
	if err := clean(g); err != nil {
		return "", err
	}
	s.mu.Lock()
	base := p.base
	s.mu.Unlock()
	if _, err := g.Run("fetch", "--quiet", "origin"); err != nil {
		return "", err
	}
	ref := "refs/remotes/origin/" + base
	if commit != "" {
		ref = commit
	}
	if _, err := g.Run("merge-base", "--is-ancestor", ref, "HEAD"); err == nil {
		return fmt.Sprintf("Already up to date with %s.", base), nil
	}
	// The merge is pushed, and the push publishes the pull request file:
	// it must be ready first, as for a commit.
	pt, err := s.readPR(p, t.Number)
	if err != nil {
		return "", err
	}
	if _, err := g.Run("merge", "--no-ff", "--no-edit", "-m", fmt.Sprintf("Merge %s into %s", base, branch(t.Number)), ref); err != nil {
		files, _ := g.Run("diff", "--name-only", "--diff-filter=U")
		if files == "" {
			return "", err
		}
		return "", fixable(fmt.Sprintf("Merging %s conflicts in these files:\n%s\n\nEdit them to resolve the conflicts, then run harness commit \"<message>\".", base, indent(files)))
	}
	out, err := s.push(p, t, g, pt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Merged %s and pushed. %s\nRun the tests: the merge can break things git cannot see.", base, out), nil
}

// run hands the caller a project command to run in the worktree.
// reply posts the bot's message in the issue, which is the conversation
// wherever the thing it answers was said. The first reply of a turn links to
// what it answers on the pull request, so the issue shows every exchange.
func (s *Service) reply(p *proj, t *store.Thread, body, dir string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fixable("write your reply on standard input: harness reply - <<'EOF' ... EOF")
	}
	if !t.Replied && t.PR != 0 {
		var links []string
		for _, e := range t.Batch {
			if e.Link != "" {
				links = append(links, e.Link)
			}
		}
		if len(links) > 0 {
			body += fmt.Sprintf("\n\n<sub>In reply to the %s on #%d.</sub>", and(links), t.PR)
		}
	}
	if err := s.post(p, t, false, t.Number, body, dir); err != nil {
		return "", err
	}
	if err := s.update(t.Repo, t.Number, func(t *store.Thread) error { t.Replied = true; return nil }); err != nil {
		return "", err
	}
	return "Replied. End the turn with harness done when you have nothing more to say or do.", nil
}

// post writes a comment on issue n, or the description of pull request n,
// uploading the images it names first.
func (s *Service) post(p *proj, t *store.Thread, pr bool, n int, body, dir string) error {
	if dir == "" {
		dir = s.worktree(p, t.Number)
	}
	body, files, err := attachments(body, dir, s.worktree(p, t.Number))
	if err != nil {
		return err
	}
	switch {
	case len(files) > 0:
		return s.gh.Upload(t.Repo, github.Upload{Gh: s.cfg.Gh, PR: pr, N: n, Body: body, Files: files})
	case pr:
		return s.gh.SetBody(t.Repo, n, body)
	}
	return s.gh.Comment(t.Repo, n, body)
}

// prBody is a pull request's description: the bot's, then what links it to
// its issue.
func prBody(text string, issue int) string {
	return fmt.Sprintf("%s\n\n---\nCloses #%d.", text, issue)
}

// done ends the turn. The pull request, if there is one, is ready for review
// until the next turn starts.
func (s *Service) done(p *proj, t *store.Thread) (string, error) {
	// A turn that only answers CI or the default branch moving on has
	// nobody to tell: what it did is in its commits and the description.
	if !t.Replied && humanAsked(t.Batch) {
		return "", fixable("nobody knows what you did or what you need: harness reply first")
	}
	g, err := s.wgit(p, t)
	if err != nil {
		return "", err
	}
	if err := clean(g); err != nil {
		return "", err
	}
	if ahead, _ := g.Run("rev-list", "--count", "refs/remotes/origin/"+branch(t.Number)+"..HEAD"); ahead != "" && ahead != "0" {
		return "", fixable("you have commits that are not pushed; run harness commit")
	}
	if t.PR != 0 && s.prSum(p, t.Number) != t.PRSource {
		return "", fixable(fmt.Sprintf("you changed %s since your last harness commit: run harness commit to publish it", prFile))
	}

	// What the turn committed has been through the whole of harness test,
	// which it may fail: the pull request then stays a draft, and the owner
	// reads why in the reply. Requiring a pass would leave a bot that cannot
	// get there stuck until it is stopped, or weakening tests to get past.
	failing := false
	if s.committedThisTurn(p, g, t) {
		head, err := g.Run("rev-parse", "HEAD^{tree}")
		if err != nil {
			return "", err
		}
		if t.TestedTree != head {
			return "", fixable("you committed this turn, and the whole of harness test has not run on what you committed: run harness test, then harness done")
		}
		failing = !t.TestedPass
	}

	out := "Turn ended. Stop here: the harness will start your next turn when there is something new."
	if failing {
		out = "Turn ended. harness test failed on what you committed, so the pull request stays a draft. Stop here."
	}
	if t.PR != 0 && !failing {
		s.mu.Lock()
		pending, err := s.pending(p, t.Number)
		s.mu.Unlock()
		if err != nil {
			return "", err
		}
		if waiting := unbatched(pending, t.Batch); waiting > 0 {
			out = fmt.Sprintf("Turn ended. %d new event(s) came in during this turn, and your next turn starts with them.", waiting)
		} else if pr, err := s.gh.PR(t.Repo, t.PR); err == nil && pr.Draft {
			if err := s.gh.SetDraft(t.Repo, pr.NodeID, false); err != nil {
				return "", err
			}
		}
	}
	if err := s.update(t.Repo, t.Number, func(t *store.Thread) error { t.Outcome = "done"; return nil }); err != nil {
		return "", err
	}
	return out, nil
}

// and joins items as English does: a, b and c.
func and(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func unbatched(pending, batch []store.Event) int {
	in := map[string]bool{}
	for _, e := range batch {
		in[e.Key] = true
	}
	n := 0
	for _, e := range pending {
		if !in[e.Key] {
			n++
		}
	}
	return n
}

// queue is the operator's view: every open thread and what it is doing.
func (s *Service) queue() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	threads, err := s.st.Threads()
	if err != nil {
		return "", err
	}
	if len(threads) == 0 {
		return "No open threads.", nil
	}
	var b strings.Builder
	for _, t := range threads {
		state := t.State
		if t.State == store.Running {
			state = fmt.Sprintf("running for %s", time.Since(t.Started).Round(time.Second))
		}
		line := fmt.Sprintf("%-30s %s", key(t.Repo, t.Number), state)
		if p := s.project(t.Repo); p != nil {
			if pending, _ := s.pending(p, t.Number); t.State != store.Running && len(pending) > 0 {
				line += fmt.Sprintf(", %d event(s) waiting", len(pending))
			} else if w := unbatched(pending, t.Batch); t.State == store.Running && w > 0 {
				line += fmt.Sprintf(", %d more waiting", w)
			}
		}
		if t.PR != 0 {
			line += fmt.Sprintf(", PR #%d", t.PR)
		}
		fmt.Fprintln(&b, line)
	}
	fmt.Fprintf(&b, "\nLogs: %s\n", filepath.Join(s.cfg.StateDir, "logs"))
	return strings.TrimRight(b.String(), "\n"), nil
}

// inScope refuses a commit that changes files outside the thread's scope.
// For a merge of the default branch, what counts is how the result differs
// from the default branch: what the merge brings in is not the bot's.
func (s *Service) inScope(p *proj, t *store.Thread, g gitpkg.Git, merging bool) error {
	sc := s.scopeOf(p, t.Scope)
	against := "HEAD"
	if merging {
		against = "MERGE_HEAD"
	}
	files, err := g.Run("diff", "--cached", "--name-only", "--no-renames", against)
	if err != nil {
		return err
	}
	var outside []string
	for _, f := range strings.Split(files, "\n") {
		if f != "" && !sc.Writes(f) {
			outside = append(outside, f)
		}
	}
	if len(outside) == 0 {
		return nil
	}
	return fixable(fmt.Sprintf("this issue is for %s, so you may change only %s, and these files are outside it:\n%s\nUndo those changes, then commit again. If the work needs them, say so with harness reply: a change elsewhere is another issue's.",
		sc, strings.Join(sc.Paths, ", "), indent(strings.Join(outside, "\n"))))
}

// scopeJSON tells the harness command what the thread works on, and what
// its branch changed against the default branch, committed or not, so that
// harness test in a turn tests what the changes affect.
func (s *Service) scopeJSON(p *proj, t *store.Thread) (string, error) {
	ts := struct {
		repo.Scope
		Changed []string `json:"changed,omitempty"`
	}{Scope: s.scopeOf(p, t.Scope)}
	if changed, err := s.changed(p, t); err == nil {
		ts.Changed = changed
	} else {
		s.log.Printf("%s: what the branch changed: %v", key(t.Repo, t.Number), err)
	}
	data, err := json.Marshal(ts)
	return string(data), err
}

// changed are the files the thread's worktree differs in from where its
// branch left the default branch: committed, staged, not, and new.
func (s *Service) changed(p *proj, t *store.Thread) ([]string, error) {
	g, err := s.wgit(p, t)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	base := p.base
	s.mu.Unlock()
	mb, err := g.Run("merge-base", "HEAD", "refs/remotes/origin/"+base)
	if err != nil {
		return nil, err
	}
	diff, err := g.Run("diff", "--name-only", "--no-renames", mb)
	if err != nil {
		return nil, err
	}
	untracked, err := g.Run("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range strings.Split(diff+"\n"+untracked, "\n") {
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out, nil
}
