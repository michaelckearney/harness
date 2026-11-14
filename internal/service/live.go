package service

import (
	"sync"
	"time"
)

// Line is one thing a bot did during a turn, as the live view shows it.
type Line struct {
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	Time   time.Time `json:"time"`
	// Kind is prompt, text, reasoning, tool, result, or error.
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// keep is how many lines of each thread the live view holds, for someone
// who starts watching partway through a turn.
const keep = 500

// hub hands each line to everyone watching, and keeps the last few of each
// thread.
type hub struct {
	mu     sync.Mutex
	subs   map[chan Line]struct{}
	recent map[string][]Line
}

func newHub() *hub {
	return &hub{subs: map[chan Line]struct{}{}, recent: map[string][]Line{}}
}

func (h *hub) publish(l Line) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := key(l.Repo, l.Number)
	r := append(h.recent[k], l)
	if len(r) > keep {
		r = r[len(r)-keep:]
	}
	h.recent[k] = r
	for c := range h.subs {
		select {
		case c <- l:
		default: // a watcher that cannot keep up misses lines, not the bot
		}
	}
}

// subscribe returns a channel of every line from now on, and a function that
// ends it.
func (h *hub) subscribe() (<-chan Line, func()) {
	c := make(chan Line, 256)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c, func() {
		h.mu.Lock()
		delete(h.subs, c)
		h.mu.Unlock()
	}
}

// lines returns the lines kept for a thread.
func (h *hub) lines(repo string, n int) []Line {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Line(nil), h.recent[key(repo, n)]...)
}
