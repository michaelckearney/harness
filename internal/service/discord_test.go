package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michaelckearney/harness/internal/discord"
	"github.com/michaelckearney/harness/internal/store"
)

// fakeChat is a Discord server, as the service uses it.
type fakeChat struct {
	mu   sync.Mutex
	next int
	msgs map[string]discord.Message // by ID
	in   map[string][]string        // what was said in each thread
	log  []string
	// history is each channel's and thread's messages, oldest first.
	history map[string][]discord.Msg
}

func newFakeChat() *fakeChat {
	return &fakeChat{msgs: map[string]discord.Message{}, in: map[string][]string{}, history: map[string][]discord.Msg{}}
}

func (c *fakeChat) id() string { c.next++; return fmt.Sprint(c.next) }

func (c *fakeChat) Post(channel string, m discord.Message) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.id()
	c.msgs[id] = m
	c.log = append(c.log, "post #"+channel+" "+m.Title)
	return id, nil
}

func (c *fakeChat) Edit(channel, id string, m discord.Message, files bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs[id] = m
	c.log = append(c.log, "edit "+id)
	return nil
}

func (c *fakeChat) Thread(channel, message, name string) (string, error) {
	return c.ThreadFrom(channel, message, name)
}

func (c *fakeChat) ThreadFrom(channel, message, name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, "thread "+message+" "+name)
	return "t" + message, nil
}

func (c *fakeChat) Say(thread, text string, files ...discord.File) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.in[thread] = append(c.in[thread], text)
	return nil
}

func (c *fakeChat) said(thread string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.in[thread]...)
}

func (c *fakeChat) Last(string) (time.Time, error) { return time.Time{}, nil }
func (c *fakeChat) History(channel string, limit int) ([]discord.Msg, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]discord.Msg(nil), c.history[channel]...), nil
}
func (c *fakeChat) Search(words string) ([]discord.Msg, error) {
	return []discord.Msg{{ID: "9", Channel: "c1", Author: "me", Content: "we said: " + words}}, nil
}
func (c *fakeChat) Read(link string) ([]discord.Msg, error) { return nil, nil }
func (c *fakeChat) Link(channel, message string) string {
	return "https://discord.test/" + channel + "/" + message
}
func (c *fakeChat) Typing(string) {}
func (c *fakeChat) Me() string    { return "1" }
func (c *fakeChat) Close()        {}

func TestPullRequestsInDiscord(t *testing.T) {
	h := setup(t)
	chat := newFakeChat()
	h.s.chat = chat
	h.s.cfg.Discord.Owner, h.s.cfg.Discord.DigestHour = "42", 99 // no digest
	h.step()
	if len(chat.log) != 0 {
		t.Fatalf("posted with no pull request: %q", chat.log)
	}
	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step() // the bot's turn opens the pull request
	h.step()
	pr := h.gh.pr("harness/" + fmt.Sprint(n))
	if pr == nil || len(chat.log) != 2 { // the message and its thread
		t.Fatalf("posts: %q", chat.log)
	}
	m := chat.msgs["1"]
	if m.Title != fmt.Sprintf("#%d feat: add hello.txt", pr.Number) || !strings.HasPrefix(m.Description, "🟡 Ready for your review") ||
		!strings.Contains(m.Description, "Adds hello.txt.") || m.Button == nil || m.Button.Disabled {
		t.Errorf("the message: %+v", m)
	}
	if got := chat.in["t1"]; len(got) != 0 { // the message shows the status; the thread is for talking
		t.Errorf("said in the thread: %q", got)
	}

	// Nothing new: nothing edited.
	h.step()
	if len(chat.log) != 2 {
		t.Errorf("edited with nothing new: %q", chat.log)
	}

	// Only the owner's Approve approves, and it is the owner's approval.
	if got := h.s.press("7", fmt.Sprintf("approve:%d", pr.Number)); got != "Only the owner can approve." {
		t.Errorf("someone else pressed Approve: %q", got)
	}
	if got := h.s.press("42", fmt.Sprintf("approve:%d", pr.Number)); !strings.HasPrefix(got, "Approved") {
		t.Fatalf("the owner pressed Approve: %q", got)
	}
	h.step()
	if got := chat.msgs["1"]; !strings.HasPrefix(got.Description, "🔵 Approved · waiting: CI has not run") || !got.Button.Disabled {
		t.Errorf("after the approval: %+v", got)
	}

	// CI passes: it merges, and CI on main is its release.
	h.gh.pass("harness/" + fmt.Sprint(n))
	h.gh.pass("main")
	h.step()
	if len(h.gh.merged()) != 1 {
		t.Fatalf("not merged: %q", chat.in["t1"])
	}
	if got := chat.msgs["1"]; !strings.HasPrefix(got.Description, "🟢 Merged · CI passed on main") || got.Button != nil {
		t.Errorf("after the merge: %+v", got)
	}
	edits := len(chat.log)
	h.step()
	if len(chat.log) != edits {
		t.Errorf("a released pull request's message changed: %q", chat.log)
	}

	// The digest has it, once a day: the bot writes it, and its link to
	// the pull request goes to the pull request's message.
	write(t, filepath.Join(h.fake, "scenario"), "digest")
	write(t, filepath.Join(h.fake, "prn"), fmt.Sprint(pr.Number))
	h.s.cfg.Discord.DigestHour = 0
	h.step()
	h.step()
	var digests []discord.Message
	for id, m := range chat.msgs {
		if strings.Contains(m.Title, "📰") {
			digests = append(digests, m)
			_ = id
		}
	}
	if len(digests) != 1 || !strings.Contains(digests[0].Title, "1 merged, 0 waiting on you") {
		t.Fatalf("digests: %+v", digests)
	}
	if want := fmt.Sprintf("Hello changed. See [#%d](https://discord.test/pull-requests/1).", pr.Number); digests[0].Description != want {
		t.Errorf("the digest says %q, want %q", digests[0].Description, want)
	}
	for _, l := range chat.log {
		if strings.HasPrefix(l, "thread") && strings.Contains(l, "📰") {
			t.Errorf("the digest has a thread: %q", l)
		}
	}
}

func TestTalkingInDiscord(t *testing.T) {
	h := setup(t)
	chat := newFakeChat()
	h.s.chat = chat
	h.s.cfg.Discord.Owner, h.s.cfg.Discord.DigestHour = "42", 99
	h.step()
	write(t, filepath.Join(h.fake, "scenario"), "discuss")
	now := time.Now()
	chat.history["c1"] = []discord.Msg{
		{ID: "998", Channel: "c1", Author: "pat", AuthorID: "7", Content: "Should the app say hi?", Time: now},
		{ID: "999", Channel: "c1", Author: "me", AuthorID: "42", Content: "Yes, I think so.", Time: now},
	}
	chat.history["t1000"] = []discord.Msg{{ID: "1000", Channel: "c1", Author: "me", AuthorID: "42", Content: "@Sparky go ahead and open an issue for it", Time: now}}

	// Someone else mentions the bot: it may read, not change GitHub.
	h.s.converse(discord.Mention{Msg: discord.Msg{ID: "1000", Channel: "c1", Author: "pat", AuthorID: "7", Content: "@Sparky open an issue", Time: now}, Where: "general"})
	if got := h.read("out"); !strings.Contains(got, "only the owner can have you change GitHub") {
		t.Errorf("open-issue for someone else:\n%s", got)
	}

	// The owner does, at the top of #general: a thread starts under the
	// message, the bot reads what was said before it, and opens the issue
	// as the owner, which a bot then works.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shot.png" {
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprint(w, "png")
			return
		}
		http.NotFound(w, r)
	}))
	defer cdn.Close()
	chat.history["t1001"] = []discord.Msg{{ID: "1001", Channel: "c1", Author: "me", AuthorID: "42", Content: "@Sparky go ahead and open an issue for it", Time: now,
		Files: []string{cdn.URL + "/shot.png?ex=1", cdn.URL + "/gone.png"}}}
	h.s.converse(discord.Mention{Msg: discord.Msg{ID: "1001", Channel: "c1", Author: "me", AuthorID: "42", Content: "@Sparky go ahead and open an issue for it", Time: now}, Where: "general"})
	prompts := h.read("prompts")
	for _, want := range []string{"me mentioned you in #general", "They are the owner", "pat: Should the app say hi?", "me: @Sparky go ahead"} {
		if !strings.Contains(prompts, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompts)
		}
	}
	// What was attached is a file the bot can open; what could not be
	// fetched keeps its link.
	shot := filepath.Join(h.s.cfg.StateDir, "attachments", "o", "r", "discord-t1001", "1001-0.png")
	if !strings.Contains(prompts, "attached: "+shot) || !strings.Contains(prompts, "not copied here") {
		t.Errorf("the prompt's attachments:\n%s", prompts)
	}
	if data, err := os.ReadFile(shot); err != nil || string(data) != "png" {
		t.Errorf("the copy: %q %v", data, err)
	}
	if got := chat.said("t1001"); len(got) != 1 || got[0] != "Opened it." {
		t.Errorf("said in the thread: %q", got)
	}
	if got := h.read("search"); !strings.Contains(got, "we said: dark mode") || !strings.Contains(got, "/c1/9") {
		t.Errorf("search: %q", got)
	}
	var n int
	h.gh.mu.Lock()
	for _, i := range h.gh.issues {
		if i.Title == "feat: say hi" {
			n = i.Number
			if i.User != "owner" || !strings.Contains(i.Body, "Add hi.txt, as we said.") || !strings.Contains(i.Body, "conversation in Discord") {
				t.Errorf("the issue: %+v", i)
			}
		}
	}
	h.gh.mu.Unlock()
	if n == 0 {
		t.Fatalf("no issue opened:\n%s", h.read("out"))
	}
	write(t, filepath.Join(h.fake, "scenario"), "quiet")
	h.step()
	if th := h.thread(n); th.Scope != "framework" {
		t.Errorf("the bot's thread for the issue: %+v", th)
	}
}

func TestDigestHelpers(t *testing.T) {
	links := map[int]string{6: "https://discord.test/pull-requests/1"}
	got := rewriteLinks("[#6](https://github.com/o/r/pull/6) and [#3](https://github.com/o/r/pull/3) and https://github.com/x/y/pull/6", "o/r", links)
	if want := "[#6](https://discord.test/pull-requests/1) and [#3](https://github.com/o/r/pull/3) and https://github.com/x/y/pull/6"; got != want {
		t.Errorf("rewriteLinks: %q", got)
	}
	if got := fmt.Sprint(pick([]string{"a", "evil", "a", "b"}, []string{"a", "b"})); got != "[a b]" {
		t.Errorf("pick: %s", got)
	}
	if !olderID("", "5") || !olderID("9", "10") || olderID("10", "9") {
		t.Error("olderID")
	}
}

func TestIssueAndPullRequestCloseTogether(t *testing.T) {
	h := setup(t)
	h.step()

	// The owner closes the issue: its pull request closes, and its branch
	// goes.
	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step()
	pr := h.gh.pr("harness/" + fmt.Sprint(n))
	if pr == nil {
		t.Fatalf("no pull request:\n%s", h.read("out"))
	}
	h.gh.close(n)
	h.step()
	if pr.State != "closed" || h.gh.branchExists("harness/"+fmt.Sprint(n)) || h.thread(n).State != store.Closed {
		t.Errorf("after the issue closed: pull request %s, branch there %v, thread %s", pr.State, h.gh.branchExists("harness/"+fmt.Sprint(n)), h.thread(n).State)
	}
	if c := h.gh.commentsOn(pr.Number); len(c) != 1 || !strings.Contains(c[0], fmt.Sprintf("its issue #%d was closed", n)) {
		t.Errorf("on the pull request: %q", c)
	}

	// The owner closes the pull request: its issue closes, as not planned.
	write(t, filepath.Join(h.fake, "scenario"), "work")
	m := h.gh.openScoped("owner", "Say hello again", "Add hello.txt, please.")
	h.step()
	pr = h.gh.pr("harness/" + fmt.Sprint(m))
	h.gh.mu.Lock()
	pr.State = "closed"
	h.gh.mu.Unlock()
	h.step()
	h.gh.mu.Lock()
	state, states := h.gh.issues[m].State, h.gh.states
	h.gh.mu.Unlock()
	if state != "closed" || !slices.Contains(states, fmt.Sprintf("%d closed not_planned app", m)) || h.gh.branchExists("harness/"+fmt.Sprint(m)) {
		t.Errorf("after the pull request closed: issue %s, changes %q", state, states)
	}
	if c := h.gh.commentsOn(m); len(c) == 0 || !strings.Contains(c[len(c)-1], fmt.Sprintf("its pull request #%d was closed", pr.Number)) {
		t.Errorf("on the issue: %q", c)
	}
}

func TestTalkingInAPullRequestsThread(t *testing.T) {
	h := setup(t)
	chat := newFakeChat()
	h.s.chat = chat
	h.s.cfg.Discord.Owner, h.s.cfg.Discord.DigestHour = "42", 99
	h.step()
	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step()
	h.step()
	pr := h.gh.pr("harness/" + fmt.Sprint(n))
	if pr == nil || !slices.Contains(chat.log, "thread 1 #"+fmt.Sprint(pr.Number)+" feat: add hello.txt") {
		t.Fatalf("no pull request, or no thread for it: %q", chat.log)
	}
	write(t, filepath.Join(h.fake, "pr"), fmt.Sprint(pr.Number))
	write(t, filepath.Join(h.fake, "issue"), fmt.Sprint(n))

	// The owner, in the pull request's thread: the bot knows which pull
	// request and issue it is, changes the issue, not the pull request,
	// and closes the issue with the owner's reason.
	write(t, filepath.Join(h.fake, "scenario"), "revise")
	now := time.Now()
	msg := discord.Msg{ID: "2000", Channel: "t1", Author: "me", AuthorID: "42", Content: "@Sparky make the dialog larger instead", Time: now}
	chat.history["t1"] = []discord.Msg{msg}
	h.s.converse(discord.Mention{Msg: msg, Thread: true, Where: "#" + fmt.Sprint(pr.Number)})
	if p := h.read("prompts"); !strings.Contains(p, fmt.Sprintf("under pull request #%d's message in #pull-requests, the pull request a bot opened for issue #%d", pr.Number, n)) {
		t.Errorf("the prompt does not name the pull request and issue:\n%s", p)
	}
	out := h.read("out")
	for _, want := range []string{
		fmt.Sprintf("#%d is a pull request, and you change issues only: a bot's pull request follows its issue, #%d", pr.Number, n),
		"say why on standard input",
		fmt.Sprintf("Closed #%d.", n),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the bot was not told %q:\n%s", want, out)
		}
	}
	if c := h.gh.commentsOn(pr.Number); len(c) != 0 {
		t.Errorf("commented on the pull request: %q", c)
	}
	h.gh.mu.Lock()
	state, states := h.gh.issues[n].State, h.gh.states
	h.gh.mu.Unlock()
	if state != "closed" || !slices.Contains(states, fmt.Sprintf("%d closed not_planned owner", n)) {
		t.Errorf("the issue is %s; changes %q", state, states)
	}
	if c := h.gh.commentsOn(n); len(c) == 0 || !strings.Contains(c[len(c)-1], "Make the dialog larger instead.") {
		t.Errorf("the reason on the issue: %q", c)
	}
	// The issue closed takes its pull request with it.
	write(t, filepath.Join(h.fake, "scenario"), "quiet")
	h.step()
	if pr.State != "closed" {
		t.Errorf("the pull request is %s", pr.State)
	}

	// Reopened, with what to do now: its bot starts again from that.
	write(t, filepath.Join(h.fake, "scenario"), "reopen")
	msg = discord.Msg{ID: "2001", Channel: "t1", Author: "me", AuthorID: "42", Content: "@Sparky actually, reopen it", Time: now}
	chat.history["t1"] = append(chat.history["t1"], msg)
	h.s.converse(discord.Mention{Msg: msg, Thread: true, Where: "#" + fmt.Sprint(pr.Number)})
	if out := h.read("out"); !strings.Contains(out, fmt.Sprintf("Reopened #%d.", n)) {
		t.Fatalf("reopen:\n%s", out)
	}
	write(t, filepath.Join(h.fake, "scenario"), "quiet")
	h.step()
	if th := h.thread(n); th.State == store.Closed {
		t.Errorf("the thread did not start again: %+v", th)
	}
	if p := h.read("prompts"); !strings.Contains(p, "Try one column after all.") {
		t.Errorf("the bot was not given the comment:\n%s", p)
	}
}
