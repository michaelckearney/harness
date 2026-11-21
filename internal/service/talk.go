package service

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/michaelckearney/harness/internal/agents"
	"github.com/michaelckearney/harness/internal/discord"
	"github.com/michaelckearney/harness/internal/github"
	"github.com/michaelckearney/harness/internal/repo"
	"github.com/michaelckearney/harness/internal/rpc"
)

//go:embed talk.md
var talkInstructions string

// specInstructions are how the talk agents write an issue, in a
// conversation and in a task alike.
//
//go:embed spec.md
var specInstructions string

// talk is a turn the bot takes on the Discord checkout: a conversation it
// was mentioned in, the morning's digest, or a run of one of the monorepo's
// tasks. One runs at a time.
type talk struct {
	kind    string // "conversation", "digest", or "task"
	thread  string // the Discord thread a conversation's replies go to
	owner   bool   // the owner mentioned the bot: it may change GitHub
	replied bool
	acted   bool     // it replied, wrote the digest, or changed GitHub
	digest  string   // the digest the bot wrote
	files   []string // the files a digest may show
	task    string   // the task a run is of
	label   string   // the label the task's issues get
	opened  int      // the issue a task's run opened
}

// conv is what the service keeps of a conversation between turns.
type conv struct {
	// Agent is the agent the session is of: it keeps the conversation
	// until it is at its limit.
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Last    string `json:"last"` // the newest message the bot has seen
}

const talkTimeout = 30 * time.Minute

// talkDir is the checkout of the default branch the bot reads in Discord.
// It is reset before each turn, and nothing in it is ever pushed.
func (s *Service) talkDir(p *proj) string {
	return filepath.Join(s.cfg.StateDir, "worktrees", p.cfg.Repo, "discord")
}

// enqueue runs a Discord turn after the ones before it, and reports whether
// it will. Without the worker, as in tests, it runs now.
func (s *Service) enqueue(job func()) bool {
	if s.jobs == nil {
		job()
		return true
	}
	select {
	case s.jobs <- job:
		return true
	default:
		s.log.Printf("discord: too much to do; dropped a turn")
		return false
	}
}

// limitWait is how long an agent at its limit is passed over when it did
// not say when the limit resets.
const limitWait = time.Hour

// talkAgents are the agents of agents.talk this server runs, in turn order:
// those not at their limit first, as harness.yaml orders them, then those
// that are, in case the limit has reset early.
func (s *Service) talkAgents(p *proj) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.repo == nil {
		return nil
	}
	var free, limited []string
	for _, name := range p.repo.Agents.Talk {
		if s.agents[name] == nil {
			continue
		}
		if time.Now().Before(s.limits[name]) {
			limited = append(limited, name)
		} else {
			free = append(free, name)
		}
	}
	return append(free, limited...)
}

// atLimit notes that an agent is at its limit until it resets.
func (s *Service) atLimit(name string, until time.Time) {
	if !until.After(time.Now()) {
		until = time.Now().Add(limitWait)
	}
	s.mu.Lock()
	s.limits[name] = until
	s.mu.Unlock()
	s.log.Printf("discord: %s is at its limit until %s", name, until.Format(time.RFC3339))
}

// talkWith runs a Discord turn with the first of the talk agents, keeping
// the agent a session is of while it is not at its limit. When an agent
// reaches its limit before the turn did anything, the next one takes the
// turn, in a new session. prompt is the turn's prompt, for a session that
// goes on or one that starts. It returns the agent and its session.
func (s *Service) talkWith(p *proj, t *talk, agent, session, title string, prompt func(resume bool) string) (string, string, error) {
	names := s.talkAgents(p)
	if len(names) == 0 {
		return "", "", fmt.Errorf("this server runs none of agents.talk")
	}
	if i := slices.Index(names, agent); i > 0 && !s.limited(agent) {
		names = append([]string{agent}, slices.Delete(slices.Clone(names), i, i+1)...)
	}
	var err error
	for _, name := range names {
		resume := name == agent && session != ""
		sess := ""
		if resume {
			sess = session
		}
		sess, err = s.talkTurn(p, t, s.agents[name], sess, title, prompt(resume))
		var lim *agents.LimitError
		if !errors.As(err, &lim) {
			return name, sess, err
		}
		s.atLimit(name, lim.Until)
		if t.acted {
			return name, sess, err
		}
	}
	return agent, session, err
}

// limited reports whether an agent is at its limit.
func (s *Service) limited(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.limits[name])
}

// prepareTalk puts the checkout at the default branch as GitHub has it.
func (s *Service) prepareTalk(p *proj) error {
	g, err := s.git(p, "")
	if err != nil {
		return err
	}
	s.mu.Lock()
	base := "refs/remotes/origin/" + p.base
	s.mu.Unlock()
	dir := s.talkDir(p)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), s.dirMode()); err != nil {
			return err
		}
		g.Run("worktree", "prune")
		_, err = g.Run("worktree", "add", "--quiet", "--detach", dir, base)
		return err
	}
	w, err := s.git(p, dir)
	if err != nil {
		return err
	}
	if _, err := w.Run("checkout", "--quiet", "--detach", "--force", base); err != nil {
		return err
	}
	_, err = w.Run("clean", "--quiet", "-fd")
	return err
}

// runTalk runs one Discord turn with the talk agents, on a fresh checkout.
func (s *Service) runTalk(p *proj, t *talk, agent, session, title string, prompt func(resume bool) string) (string, string, error) {
	if err := s.prepareTalk(p); err != nil {
		return agent, session, fmt.Errorf("checking out %s: %w", p.base, err)
	}
	return s.talkWith(p, t, agent, session, title, prompt)
}

// talkTurn runs one Discord turn with agent a, on the checkout as it is.
func (s *Service) talkTurn(p *proj, t *talk, a agents.Agent, session, title, prompt string) (string, error) {
	s.mu.Lock()
	s.talking = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.talking = nil
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), talkTimeout)
	defer cancel()
	return a.Run(ctx, agents.Turn{
		Dir: s.talkDir(p), Session: session, Title: title, Prompt: prompt, System: s.talkSystem(p, t.kind),
		Say: func(kind, text string) { s.log.Printf("discord: %s: [%s] %s", title, kind, compactLine(text)) },
	})
}

// talkSystem is the instructions for a Discord turn: the harness's, for a
// conversation and the digest or for a task, then the monorepo's own
// AGENTS.md and the framework's, read as files.
func (s *Service) talkSystem(p *proj, kind string) string {
	var b strings.Builder
	instructions := talkInstructions
	if kind == "task" {
		instructions = taskInstructions
	}
	b.WriteString(strings.ReplaceAll(instructions, "$HARNESS", s.Command))
	if kind != "digest" {
		b.WriteString("\n" + specInstructions)
	}
	s.mu.Lock()
	cfg := p.repo
	s.mu.Unlock()
	folders := []string{"."}
	if cfg != nil {
		folders = append(folders, cfg.Framework)
	}
	for _, f := range folders {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			data, err := os.ReadFile(filepath.Join(s.talkDir(p), f, name))
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "\n## %s\n\n%s\n", filepath.Join(f, name), strings.TrimSpace(string(data)))
			break
		}
	}
	return b.String()
}

// converse answers a message that mentions the bot: in its thread, or in a
// thread it starts under the message.
func (s *Service) converse(m discord.Mention) {
	p := s.projects[0]
	thread := m.Channel
	if !m.Thread {
		name := strings.TrimSpace(strings.SplitN(m.Content, "\n", 2)[0])
		if name == "" {
			name = "Conversation"
		}
		var err error
		if thread, err = s.chat.ThreadFrom(m.Channel, m.ID, truncate(name, 80)); err != nil {
			s.log.Printf("discord: starting a thread: %v", err)
			return
		}
	}
	key := "discord:conv:" + thread
	var c conv
	if text, _ := s.st.Get(key); text != "" {
		json.Unmarshal([]byte(text), &c)
	}

	history, err := s.chat.History(thread, 100)
	if err != nil {
		s.log.Printf("discord: reading the thread: %v", err)
		return
	}
	// A conversation the bot was brought into at the top of a channel
	// starts with what was said there just before.
	if !m.Thread && c.Session == "" {
		if before, err := s.chat.History(m.Channel, 20); err == nil {
			var earlier []discord.Msg
			for _, msg := range before {
				if olderID(msg.ID, m.ID) {
					earlier = append(earlier, msg)
				}
			}
			history = append(earlier, history...)
		}
	}
	me := s.chat.Me()
	last := c.Last
	for _, msg := range history {
		if olderID(last, msg.ID) {
			last = msg.ID
		}
	}
	// lines are what the prompt shows of the thread: what was said since
	// the bot's last reply, for a session that goes on, or all of it, with
	// the bot's own replies, for one that starts.
	lines := func(resume bool) []string {
		var lines []string
		for _, msg := range history {
			if resume && !olderID(c.Last, msg.ID) {
				continue
			}
			if msg.AuthorID == me {
				if !resume {
					lines = append(lines, fmt.Sprintf("[%s] you: %s", msg.Time.Format("2006-01-02 15:04"), msg.Content))
				}
				continue
			}
			lines = append(lines, s.talkLine(p, thread, msg))
		}
		if len(lines) == 0 {
			lines = append(lines, s.talkLine(p, thread, m.Msg))
		}
		return lines
	}

	owner := m.AuthorID == s.cfg.Discord.Owner
	where := "#" + m.Where
	if m.Thread {
		where = "the thread " + m.Where
	}
	var head strings.Builder
	prompt := &head
	fmt.Fprintf(prompt, "%s mentioned you in %s.\n", m.Author, where)
	if owner {
		prompt.WriteString("They are the owner: if they tell you to, you may open issues, comment, and label on GitHub.\n")
	} else {
		prompt.WriteString("They are not the owner: you cannot open issues, comment, or label on GitHub this turn; say what you would do, for the owner to confirm.\n")
	}
	if pr := s.prOfThread(thread); pr != 0 {
		fmt.Fprintf(prompt, "This thread is under pull request #%d's message in #pull-requests", pr)
		if n := s.issueOf(p, pr); n != 0 {
			fmt.Fprintf(prompt, ", the pull request a bot opened for issue #%d", n)
		}
		prompt.WriteString(": what is said here is about them. Read them with issue <n>, and the pull request's code with changes <n>.\n")
	}
	promptFor := func(resume bool) string {
		var b strings.Builder
		b.WriteString(head.String())
		if resume {
			b.WriteString("\nWhat was said since your last reply, oldest first:\n\n")
		} else {
			b.WriteString("\nThe conversation so far, oldest first:\n\n")
		}
		b.WriteString(strings.Join(lines(resume), "\n"))
		b.WriteString("\n\nAnswer with `" + s.Command + " reply -`.\n")
		return b.String()
	}

	t := &talk{kind: "conversation", thread: thread, owner: owner}
	stop := make(chan struct{})
	go func() {
		for {
			s.chat.Typing(thread)
			select {
			case <-stop:
				return
			case <-time.After(8 * time.Second):
			}
		}
	}()
	agent, session, err := s.runTalk(p, t, c.Agent, c.Session, "discord-"+thread, promptFor)
	close(stop)
	if err != nil {
		s.log.Printf("discord: conversation %s: %v", thread, err)
	}
	if !t.replied {
		s.chat.Say(thread, "I could not put an answer together that time. Mention me again to have another go.")
	}
	c.Agent, c.Session, c.Last = agent, session, last
	if data, err := json.Marshal(c); err == nil {
		s.st.Set(key, string(data))
	}
}

// talkLine is a message as a Discord turn's prompt shows it. The images
// and videos attached to it are copied to this machine, where the bot can
// open them with its read tool: it cannot fetch a link, and Discord's links
// expire within a day. A file that is neither, or cannot be fetched, keeps
// its link.
func (s *Service) talkLine(p *proj, thread string, m discord.Msg) string {
	line := fmt.Sprintf("[%s] %s: %s", m.Time.Format("2006-01-02 15:04"), m.Author, m.Content)
	dir := filepath.Join(s.cfg.StateDir, "attachments", p.cfg.Repo, "discord-"+thread)
	for i, u := range m.Files {
		file, err := fetchAttached(dir, fmt.Sprintf("%s-%d", m.ID, i), u, attachedClient.Get)
		if err != nil {
			line += "\n  attached, and not copied here (" + err.Error() + "): " + u
			continue
		}
		line += "\n  attached: " + file
	}
	return line
}

// olderID reports whether Discord's ID a is older than b: snowflakes grow.
func olderID(a, b string) bool {
	if a == "" {
		return true
	}
	x, err1 := strconv.ParseUint(a, 10, 64)
	y, err2 := strconv.ParseUint(b, 10, 64)
	return err1 == nil && err2 == nil && x < y
}

// talkCommands are the commands of a Discord turn, for the bot's help.
const talkCommands = "In Discord, the commands are: reply -, issues, issue <n>, changes <n>, search-discord <words>, read-discord <link>, " +
	"open-issue [-label <label>]... -, comment <n> -, label <n> <label>..., close <n> -, reopen <n> -, digest -, and done."

// acts are the Discord turn's commands that leave something behind: a turn
// that ran one is not taken again by another agent.
var acts = []string{"reply", "digest", "open-issue", "comment", "label", "close", "reopen"}

// handleTalk answers a command run in the Discord checkout, during a
// Discord turn.
func (s *Service) handleTalk(p *proj, req rpc.Request) (out string, err error) {
	s.mu.Lock()
	t := s.talking
	s.mu.Unlock()
	if t == nil {
		return "", fixable("there is no Discord turn running; the harness only acts during one")
	}
	defer func() {
		if err == nil && slices.Contains(acts, req.Cmd) {
			t.acted = true
		}
	}()
	needOwner := func() error {
		if t.kind == "task" {
			return fixable("a task opens issues, and changes nothing else on GitHub")
		}
		if t.kind != "conversation" || !t.owner {
			return fixable("only the owner can have you change GitHub, and the owner did not ask this turn; say what you would do instead, for them to confirm")
		}
		return nil
	}
	number := func() (int, error) {
		if len(req.Args) == 0 {
			return 0, fixable("give the issue or pull request's number")
		}
		n, err := strconv.Atoi(strings.TrimPrefix(req.Args[0], "#"))
		if err != nil {
			return 0, fixable(fmt.Sprintf("%q is not a number", req.Args[0]))
		}
		return n, nil
	}
	// anIssue is issue n, which is not a pull request: the Discord bot
	// changes issues only, and a bot's pull request follows its issue.
	anIssue := func(n int) (*github.Issue, error) {
		issue, err := s.gh.Issue(p.cfg.Repo, n)
		if err != nil {
			return nil, fixable(fmt.Sprintf("#%d: %v", n, err))
		}
		if issue.PullRequest != nil {
			msg := fmt.Sprintf("#%d is a pull request, and you change issues only: a bot's pull request follows its issue", n)
			if i := s.issueOf(p, n); i != 0 {
				msg += fmt.Sprintf(", #%d", i)
			}
			return nil, fixable(msg)
		}
		return issue, nil
	}
	footer := ""
	switch {
	case t.thread != "":
		footer = fmt.Sprintf("\n\n---\n<sub>From a [conversation in Discord](https://discord.com/channels/%s/%s).</sub>", s.cfg.Discord.Server, t.thread)
	case t.kind == "task":
		footer = fmt.Sprintf("\n\n---\n<sub>From the %s task.</sub>", t.task)
	}
	discordOnly := func() error {
		if s.chat == nil {
			return fixable("this monorepo has no Discord server")
		}
		return nil
	}
	switch req.Cmd {
	case "status", "help":
		if t.kind == "task" {
			return taskCommands, nil
		}
		return talkCommands, nil
	case "done":
		return "Turn ended. Stop here.", nil
	case "scope":
		// A framework verb run here, such as record, is for the framework
		// and every project, unless it names one with -p.
		data, err := json.Marshal(s.scopeOf(p, repo.FrameworkKind))
		return string(data), err
	case "reply":
		if t.kind == "task" {
			return "", fixable("a task has nobody to reply to: open an issue, or end the turn with done")
		}
		if t.kind != "conversation" {
			return "", fixable("this is the digest: write it with digest -, not reply")
		}
		return s.talkReply(p, t, req.Body, req.BodyDir)
	case "digest":
		if t.kind != "digest" {
			return "", fixable("digest is for the morning's digest; in a conversation, answer with reply -")
		}
		if strings.TrimSpace(req.Body) == "" {
			return "", fixable("write the digest on standard input: harness digest - <<'EOF' ... EOF")
		}
		t.digest = strings.TrimSpace(req.Body)
		return "Taken. It is posted when your turn ends; stop here.", nil
	case "issues":
		issues, err := s.gh.OpenIssues(p.cfg.Repo)
		if err != nil {
			return "", err
		}
		var lines []string
		for _, i := range issues {
			kind := "issue"
			if i.PullRequest != nil {
				kind = "pull request"
			}
			var labels []string
			for _, l := range i.Labels {
				labels = append(labels, l.Name)
			}
			lines = append(lines, fmt.Sprintf("#%d %s: %s, by %s, updated %s [%s]", i.Number, kind, i.Title, i.User.Login,
				i.UpdatedAt.Format("2006-01-02"), strings.Join(labels, ", ")))
		}
		if len(lines) == 0 {
			return "No issues or pull requests are open.", nil
		}
		return strings.Join(lines, "\n"), nil
	case "issue":
		n, err := number()
		if err != nil {
			return "", err
		}
		return s.describeIssue(p, n)
	case "changes":
		n, err := number()
		if err != nil {
			return "", err
		}
		return s.describeChanges(p, n)
	case "search-discord":
		if err := discordOnly(); err != nil {
			return "", err
		}
		if len(req.Args) == 0 {
			return "", fixable("say what to search for: harness search-discord dark mode")
		}
		msgs, err := s.chat.Search(strings.Join(req.Args, " "))
		if err != nil {
			return "", err
		}
		if len(msgs) == 0 {
			return "Nothing in the server matches.", nil
		}
		var lines []string
		for _, m := range msgs {
			lines = append(lines, fmt.Sprintf("[%s] %s: %s\n  https://discord.com/channels/%s/%s/%s",
				m.Time.Format("2006-01-02 15:04"), m.Author, compactLine(m.Content), s.cfg.Discord.Server, m.Channel, m.ID))
		}
		return strings.Join(lines, "\n") + "\n\nRead a whole conversation with read-discord <link>.", nil
	case "read-discord":
		if err := discordOnly(); err != nil {
			return "", err
		}
		if len(req.Args) != 1 {
			return "", fixable("give one link: harness read-discord https://discord.com/channels/...")
		}
		msgs, err := s.chat.Read(req.Args[0])
		if err != nil {
			return "", fixable(err.Error())
		}
		var lines []string
		for _, m := range msgs {
			lines = append(lines, s.talkLine(p, m.Channel, m))
		}
		return strings.Join(lines, "\n"), nil
	case "open-issue":
		if t.kind != "task" {
			if err := needOwner(); err != nil {
				return "", err
			}
		}
		title, body, _ := strings.Cut(strings.TrimSpace(req.Body), "\n")
		if strings.TrimSpace(title) == "" {
			return "", fixable("on standard input, the title on the first line, then the description")
		}
		var labels []string
		for i := 0; i < len(req.Args); i++ {
			if req.Args[i] == "-label" || req.Args[i] == "--label" {
				if i+1 < len(req.Args) {
					labels = append(labels, req.Args[i+1])
					i++
				}
			}
		}
		if t.kind == "task" {
			var err error
			if labels, err = s.taskLabels(p, t, labels); err != nil {
				return "", err
			}
		}
		// The images and videos it shows from the checkout go up first, as
		// a reply's do.
		dir := req.BodyDir
		if dir == "" {
			dir = s.talkDir(p)
		}
		body, files, err := attachments(strings.TrimSpace(body), dir, s.talkDir(p))
		if err != nil {
			return "", err
		}
		if len(files) > 0 {
			if body, err = s.gh.UploadFiles(p.cfg.Repo, s.cfg.Gh, body, files); err != nil {
				return "", err
			}
		}
		tok, err := github.OwnerToken(s.cfg.Gh)
		if err != nil {
			return "", err
		}
		issue, err := s.gh.OpenIssue(p.cfg.Repo, strings.TrimSpace(title), body+footer, labels, tok)
		if err != nil {
			return "", err
		}
		if t.kind == "task" {
			t.opened = issue.Number
			s.log.Printf("task %s: opened #%d: %s", t.task, issue.Number, title)
			return fmt.Sprintf("Opened #%d: %s\nA bot starts on it soon. End the turn with done.", issue.Number, issue.HTMLURL), nil
		}
		s.log.Printf("discord: opened #%d for the owner: %s", issue.Number, title)
		msg := fmt.Sprintf("Opened #%d: %s", issue.Number, issue.HTMLURL)
		if len(labels) == 0 {
			msg += "\nIt has no scope label, so no bot works it until it gets one."
		}
		return msg, nil
	case "comment":
		if err := needOwner(); err != nil {
			return "", err
		}
		n, err := number()
		if err != nil {
			return "", err
		}
		if _, err := anIssue(n); err != nil {
			return "", err
		}
		if strings.TrimSpace(req.Body) == "" {
			return "", fixable("write the comment on standard input: harness comment 12 - <<'EOF' ... EOF")
		}
		tok, err := github.OwnerToken(s.cfg.Gh)
		if err != nil {
			return "", err
		}
		if err := s.gh.CommentAs(p.cfg.Repo, n, strings.TrimSpace(req.Body)+footer, tok); err != nil {
			return "", err
		}
		s.log.Printf("discord: commented on #%d for the owner", n)
		return fmt.Sprintf("Commented on #%d.", n), nil
	case "label":
		if err := needOwner(); err != nil {
			return "", err
		}
		n, err := number()
		if err != nil {
			return "", err
		}
		if len(req.Args) < 2 {
			return "", fixable("give the labels: harness label 12 project:harbor")
		}
		if _, err := anIssue(n); err != nil {
			return "", err
		}
		tok, err := github.OwnerToken(s.cfg.Gh)
		if err != nil {
			return "", err
		}
		if err := s.gh.Label(p.cfg.Repo, n, req.Args[1:], tok); err != nil {
			return "", err
		}
		return fmt.Sprintf("Labelled #%d %s.", n, strings.Join(req.Args[1:], ", ")), nil
	case "close", "reopen":
		if err := needOwner(); err != nil {
			return "", err
		}
		n, err := number()
		if err != nil {
			return "", err
		}
		issue, err := anIssue(n)
		if err != nil {
			return "", err
		}
		closing := req.Cmd == "close"
		if closing != (issue.State == "open") {
			return "", fixable(fmt.Sprintf("#%d is %s already", n, issue.State))
		}
		why := strings.TrimSpace(req.Body)
		if why == "" {
			if closing {
				return "", fixable("say why on standard input, in the owner's words: harness close 12 - <<'EOF' ... EOF")
			}
			return "", fixable("say what to do now on standard input, in the owner's words: harness reopen 12 - <<'EOF' ... EOF")
		}
		tok, err := github.OwnerToken(s.cfg.Gh)
		if err != nil {
			return "", err
		}
		if closing {
			// The reason first, so it is on the issue when it closes.
			if err := s.gh.CommentAs(p.cfg.Repo, n, why+footer, tok); err != nil {
				return "", err
			}
			if err := s.gh.SetState(p.cfg.Repo, n, "closed", "not_planned", tok); err != nil {
				return "", err
			}
			s.log.Printf("discord: closed #%d for the owner", n)
			return fmt.Sprintf("Closed #%d. Its pull request, if it has one, closes with it.", n), nil
		}
		// Reopened, then the comment, which is new to the issue's bot: it
		// starts again on what it says.
		if err := s.gh.SetState(p.cfg.Repo, n, "open", "", tok); err != nil {
			return "", err
		}
		if err := s.gh.CommentAs(p.cfg.Repo, n, why+footer, tok); err != nil {
			return "", err
		}
		s.log.Printf("discord: reopened #%d for the owner", n)
		return fmt.Sprintf("Reopened #%d. A bot starts on it again, from your comment.", n), nil
	}
	return "", fixable(fmt.Sprintf("%q is not a command in Discord. %s", req.Cmd, talkCommands))
}

var localImage = regexp.MustCompile(`!\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)

// talkReply posts the bot's reply in its thread, with the files from the
// checkout it shows attached.
func (s *Service) talkReply(p *proj, t *talk, body, dir string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fixable("write your reply on standard input: harness reply - <<'EOF' ... EOF")
	}
	if dir == "" {
		dir = s.talkDir(p)
	}
	_, paths, err := attachments(body, dir, s.talkDir(p))
	if err != nil {
		return "", err
	}
	var files []discord.File
	for i, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || len(data) > maxFileSize {
			continue
		}
		ext := strings.ToLower(filepath.Ext(path))
		files = append(files, discord.File{Name: fmt.Sprintf("%d%s", i+1, ext), ContentType: github.ContentTypes[ext], Data: data})
	}
	if len(paths) > 0 {
		body = strings.TrimSpace(localImage.ReplaceAllStringFunc(body, func(m string) string {
			if strings.HasPrefix(localImage.FindStringSubmatch(m)[1], "http") {
				return m
			}
			return ""
		}))
	}
	if err := s.chat.Say(t.thread, body, files...); err != nil {
		return "", err
	}
	t.replied = true
	return "Replied. Stop here unless you have more to say.", nil
}

// describeIssue is an issue or pull request, and what was said on it.
func (s *Service) describeIssue(p *proj, n int) (string, error) {
	issue, err := s.gh.Issue(p.cfg.Repo, n)
	if err != nil {
		return "", fixable(fmt.Sprintf("#%d: %v", n, err))
	}
	kind := "Issue"
	if issue.PullRequest != nil {
		kind = "Pull request"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s #%d: %s\n%s, by %s, %s\n\n%s\n", kind, n, issue.Title, issue.State, issue.User.Login, issue.HTMLURL, strings.TrimSpace(issue.Body))
	comments, err := s.gh.Comments(p.cfg.Repo, n)
	if err != nil {
		return "", err
	}
	if len(comments) > 30 {
		fmt.Fprintf(&b, "\n(%d earlier comments left out)\n", len(comments)-30)
		comments = comments[len(comments)-30:]
	}
	for _, c := range comments {
		fmt.Fprintf(&b, "\n--- %s, %s:\n%s\n", c.User.Login, c.CreatedAt.Format("2006-01-02 15:04"), strings.TrimSpace(c.Body))
	}
	return b.String(), nil
}

// changesLimit is about how much of a pull request's diff changes shows.
const changesLimit = 60_000

// describeChanges is the files a pull request changes, and their diffs, so
// the talk bot can read a change it cannot check out.
func (s *Service) describeChanges(p *proj, n int) (string, error) {
	pr, err := s.gh.PR(p.cfg.Repo, n)
	if err != nil {
		return "", fixable(fmt.Sprintf("#%d: %v", n, err))
	}
	files, err := s.gh.PRFiles(p.cfg.Repo, n)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Pull request #%d: %s\n%d files changed\n\n", n, pr.Title, len(files))
	for _, f := range files {
		fmt.Fprintf(&b, "%s %s (+%d -%d)\n", f.Status, f.Filename, f.Additions, f.Deletions)
	}
	for i, f := range files {
		if b.Len() > changesLimit {
			fmt.Fprintf(&b, "\n(the diffs of the last %d files are left out: the change is too large to show whole)\n", len(files)-i)
			break
		}
		if f.Patch == "" {
			fmt.Fprintf(&b, "\n--- %s: no diff to show (binary, or too large)\n", f.Filename)
			continue
		}
		fmt.Fprintf(&b, "\n--- %s\n%s\n", f.Filename, f.Patch)
	}
	return b.String(), nil
}

// prOfThread is the pull request whose message in #pull-requests thread is
// under, or 0.
func (s *Service) prOfThread(thread string) int {
	posts, err := s.posts()
	if err != nil {
		return 0
	}
	for n, po := range posts {
		if po.Thread == thread {
			return n
		}
	}
	return 0
}

// issueOf is the issue whose bot's pull request is pr, or 0.
func (s *Service) issueOf(p *proj, pr int) int {
	threads, err := s.st.Threads()
	if err != nil {
		return 0
	}
	for _, t := range threads {
		if t.Repo == p.cfg.Repo && t.PR == pr {
			return t.Number
		}
	}
	return 0
}

// inTalkDir reports whether a command ran in the Discord checkout.
func (s *Service) inTalkDir(p *proj, cwd string) bool {
	dir, err1 := filepath.EvalSymlinks(cwd)
	root, err2 := filepath.EvalSymlinks(s.talkDir(p))
	return err1 == nil && err2 == nil && (dir == root || strings.HasPrefix(dir, root+string(filepath.Separator)))
}

func compactLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, 300)
}

// pick keeps the URLs of a digest that are among the files it may show, at
// most maxFiles, in order and once each.
func pick(urls, allowed []string) []string {
	var out []string
	for _, u := range urls {
		if slices.Contains(allowed, u) && !slices.Contains(out, u) && len(out) < maxFiles {
			out = append(out, u)
		}
	}
	return out
}
