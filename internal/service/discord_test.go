package service

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michaelckearney/harness/internal/discord"
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
	if got := chat.in["t1"]; len(got) != 1 || !strings.HasPrefix(got[0], "🟡 Ready for your review") {
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
	chat.history["t1001"] = []discord.Msg{{ID: "1001", Channel: "c1", Author: "me", AuthorID: "42", Content: "@Sparky go ahead and open an issue for it", Time: now}}
	h.s.converse(discord.Mention{Msg: discord.Msg{ID: "1001", Channel: "c1", Author: "me", AuthorID: "42", Content: "@Sparky go ahead and open an issue for it", Time: now}, Where: "general"})
	prompts := h.read("prompts")
	for _, want := range []string{"me mentioned you in #general", "They are the owner", "pat: Should the app say hi?", "me: @Sparky go ahead"} {
		if !strings.Contains(prompts, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompts)
		}
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
