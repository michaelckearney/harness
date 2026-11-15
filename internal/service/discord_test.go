package service

import (
	"fmt"
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
}

func newFakeChat() *fakeChat {
	return &fakeChat{msgs: map[string]discord.Message{}, in: map[string][]string{}}
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
	c.mu.Lock()
	defer c.mu.Unlock()
	return "t" + message, nil
}

func (c *fakeChat) Say(thread, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.in[thread] = append(c.in[thread], text)
	return nil
}

func (c *fakeChat) SayMessage(thread string, m discord.Message) error {
	return c.Say(thread, m.Title+" ["+m.Button.Label+"]")
}

func (c *fakeChat) Last(string) (time.Time, error) { return time.Time{}, nil }
func (c *fakeChat) Close()                         {}

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
	if pr == nil || len(chat.log) != 1 {
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
	if len(chat.log) != 1 {
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

	// The digest has it, once a day.
	h.s.cfg.Discord.DigestHour = 0
	h.step()
	h.step()
	var digests []string
	for _, l := range chat.log {
		if strings.HasPrefix(l, "post #daily-digest") {
			digests = append(digests, l)
		}
	}
	if len(digests) != 1 || !strings.Contains(digests[0], "1 merged, 0 waiting on you") {
		t.Errorf("digests: %q", digests)
	}
}
