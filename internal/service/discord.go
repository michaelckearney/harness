package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michaelckearney/harness/internal/discord"
	"github.com/michaelckearney/harness/internal/github"
	"github.com/michaelckearney/harness/internal/store"
)

// chat is the monorepo's Discord server: discord.Bot, or a test's fake.
type chat interface {
	Post(channel string, m discord.Message) (string, error)
	Edit(channel, id string, m discord.Message, files bool) error
	Thread(channel, message, name string) (string, error)
	Say(thread, text string) error
	SayMessage(thread string, m discord.Message) error
	Last(channel string) (time.Time, error)
	Close()
}

const (
	prChannel     = "pull-requests"
	digestChannel = "daily-digest"
	postsKey      = "discord:prs"
	digestKey     = "discord:digest"
	maxFiles      = 10
	maxFileSize   = 10 << 20 // what a server that is not boosted takes
)

// Colours of a pull request's message, by where it is.
const (
	yellow = 0xf0b232
	blue   = 0x5865f2
	purple = 0x9b59b6
	green  = 0x57f287
	red    = 0xed4245
	grey   = 0x80848e
)

// post is a pull request's message in #pull-requests.
type post struct {
	Message string   `json:"message"`
	Thread  string   `json:"thread"`
	Status  string   `json:"status"` // the status line it last showed
	Shown   string   `json:"shown"`  // a hash of what it last showed
	Files   []string `json:"files"`  // the URLs of the files it shows
	Done    bool     `json:"done"`   // merged and released, or closed: it changes no more
}

// openDiscord connects to the Discord server, if there is one.
func (s *Service) openDiscord() error {
	d := s.cfg.Discord
	if d.Server == "" {
		return nil
	}
	tok, err := os.ReadFile(d.TokenFile)
	if err != nil {
		return err
	}
	bot, err := discord.Open(strings.TrimSpace(string(tok)), d.Server, s.press)
	if err != nil {
		return err
	}
	s.chat = bot
	s.log.Printf("discord: connected to server %s", d.Server)
	return nil
}

// press answers a button in Discord: Approve, which only the owner may
// press, approves the pull request as the owner's gh login.
func (s *Service) press(user, id string) string {
	n, ok := strings.CutPrefix(id, "approve:")
	pr, err := strconv.Atoi(n)
	if !ok || err != nil {
		return "That button does nothing."
	}
	if user != s.cfg.Discord.Owner {
		return "Only the owner can approve."
	}
	tok, err := github.OwnerToken(s.cfg.Gh)
	if err == nil {
		err = s.gh.Approve(s.cfg.Repo, pr, tok)
	}
	if err != nil {
		s.log.Printf("discord: approving #%d: %v", pr, err)
		return fmt.Sprintf("Could not approve #%d: %v", pr, err)
	}
	s.log.Printf("discord: the owner approved #%d", pr)
	return fmt.Sprintf("Approved #%d. It merges once it is ready.", pr)
}

func (s *Service) posts() (map[int]*post, error) {
	out := map[int]*post{}
	text, err := s.st.Get(postsKey)
	if err != nil || text == "" {
		return out, err
	}
	return out, json.Unmarshal([]byte(text), &out)
}

func (s *Service) savePosts(posts map[int]*post) error {
	data, err := json.Marshal(posts)
	if err != nil {
		return err
	}
	return s.st.Set(postsKey, string(data))
}

// notify keeps #pull-requests up to date: one message for each of the bots'
// pull requests, which shows where it is, its description and its files,
// and a thread under it where each change of where it is is said. The owner
// is pinged in the thread when its release fails.
func (s *Service) notify(p *proj) {
	if s.chat == nil {
		return
	}
	posts, err := s.posts()
	if err != nil {
		s.log.Printf("discord: %v", err)
		return
	}
	threads, err := s.st.Threads()
	if err != nil {
		s.log.Printf("discord: %v", err)
		return
	}
	byPR := map[int]*store.Thread{}
	for _, t := range threads {
		if t.Repo == p.cfg.Repo && t.PR != 0 {
			byPR[t.PR] = t
			if posts[t.PR] == nil && t.State != store.Closed {
				posts[t.PR] = &post{}
			}
		}
	}
	var prs []int
	for n, po := range posts {
		if !po.Done {
			prs = append(prs, n)
		}
	}
	sort.Ints(prs)
	for _, n := range prs {
		if err := s.notifyPR(p, posts[n], byPR[n], n); err != nil {
			s.log.Printf("discord: #%d: %v", n, err)
		}
	}
	if err := s.savePosts(posts); err != nil {
		s.log.Printf("discord: %v", err)
	}
	if err := s.digest(p, posts, byPR); err != nil {
		s.log.Printf("discord: digest: %v", err)
	}
}

// notifyPR brings one pull request's message up to date.
func (s *Service) notifyPR(p *proj, po *post, t *store.Thread, n int) error {
	pr, err := s.gh.PR(p.cfg.Repo, n)
	if err != nil {
		return err
	}
	status, color, button, failed, done, err := s.prStatus(p, pr, t)
	if err != nil {
		return err
	}
	m, overflow, urls := message(pr, status, color, button)
	shown := hash(m.Title, m.Description, fmt.Sprint(color), fmt.Sprint(button), strings.Join(urls, " "))
	if shown == po.Shown {
		po.Done = done
		return nil
	}
	files := !slices.Equal(urls, po.Files)
	if files || po.Message == "" {
		m.Files = s.download(urls)
	}
	if po.Message == "" {
		id, err := s.chat.Post(prChannel, m)
		if err != nil {
			return err
		}
		po.Message = id
		if po.Thread, err = s.chat.Thread(prChannel, id, fmt.Sprintf("#%d %s", pr.Number, pr.Title)); err != nil {
			return err
		}
		for _, part := range overflow {
			if err := s.chat.Say(po.Thread, part); err != nil {
				return err
			}
		}
	} else if err := s.chat.Edit(prChannel, po.Message, m, files); err != nil {
		return err
	}
	if status != po.Status && po.Thread != "" {
		text := status
		if failed {
			text = fmt.Sprintf("<@%s> %s", s.cfg.Discord.Owner, status)
		}
		if err := s.chat.Say(po.Thread, text); err != nil {
			return err
		}
	}
	po.Status, po.Shown, po.Files, po.Done = status, shown, urls, done
	return nil
}

// prStatus is where a pull request is, as its message says: its status line,
// colour, and button; whether it failed in a way the owner must hear of;
// and whether it changes no more.
func (s *Service) prStatus(p *proj, pr *github.PR, t *store.Thread) (string, int, *discord.Button, bool, bool, error) {
	approve := &discord.Button{ID: fmt.Sprintf("approve:%d", pr.Number), Label: "Approve"}
	switch {
	case pr.MergedAt != nil:
		runs, err := s.gh.CheckRuns(p.cfg.Repo, pr.MergeCommit)
		if err != nil {
			return "", 0, nil, false, false, err
		}
		var bad []string
		for _, r := range runs {
			if r.Status != "completed" {
				return "🟣 Merged · releasing", purple, nil, false, false, nil
			}
			if !slices.Contains(passed, r.Conclusion) {
				bad = append(bad, fmt.Sprintf("[%s](%s) %s", r.Name, r.HTMLURL, r.Conclusion))
			}
		}
		if len(runs) == 0 {
			return "🟣 Merged · releasing", purple, nil, false, false, nil
		}
		if len(bad) > 0 {
			return "🔴 Merged · the release failed: " + strings.Join(bad, ", "), red, nil, true, true, nil
		}
		var released []string
		if tags, err := s.gh.Tags(p.cfg.Repo); err == nil {
			for _, tag := range tags {
				if tag.Commit.SHA == pr.MergeCommit {
					released = append(released, "`"+tag.Name+"`")
				}
			}
		}
		if len(released) == 0 {
			return "🟢 Merged · CI passed on main", green, nil, false, true, nil
		}
		return "🟢 Released " + strings.Join(released, ", "), green, nil, false, true, nil
	case pr.State != "open":
		return "⚫ Closed without merging", grey, nil, false, true, nil
	}

	s.mu.Lock()
	running := t != nil && s.turns[key(t.Repo, t.Number)] != nil
	s.mu.Unlock()
	if running || pr.Draft {
		return "🟡 The bot is working on it", yellow, nil, false, false, nil
	}
	reviews, err := s.gh.Reviews(p.cfg.Repo, pr.Number)
	if err != nil {
		return "", 0, nil, false, false, err
	}
	if len(approvals(reviews, p.repo.Trusted)) == 0 {
		ci := ""
		if runs, err := s.gh.CheckRuns(p.cfg.Repo, pr.Head.SHA); err == nil {
			for _, r := range runs {
				if r.Status != "completed" {
					ci = " · CI is running"
					break
				}
				if !slices.Contains(passed, r.Conclusion) {
					ci = " · CI failed"
				}
			}
		}
		return "🟡 Ready for your review" + ci, yellow, approve, false, false, nil
	}
	approve.Label, approve.Disabled = "Approved", true
	if t == nil {
		return "🔵 Approved", blue, approve, false, false, nil
	}
	_, why, err := s.mergeable(p, t)
	if err != nil {
		return "", 0, nil, false, false, err
	}
	if why == "" {
		return "🔵 Approved · merging", blue, approve, false, false, nil
	}
	return "🔵 Approved · waiting: " + why, blue, approve, false, false, nil
}

// message is a pull request's message: its status, then its description
// as far as an embed holds it, with the rest to say in its thread, and the
// URLs of the files it shows.
func message(pr *github.PR, status string, color int, button *discord.Button) (discord.Message, []string, []string) {
	text, urls := discord.Translate(pr.Body)
	if len(urls) > maxFiles {
		urls = urls[:maxFiles]
	}
	head := status + "\n\n"
	parts := discord.Split(text, discord.Limit-len(head)-50, 1900)
	desc := head + parts[0]
	if len(parts) > 1 {
		desc += "\n\n*…continued in the thread.*"
	}
	m := discord.Message{Title: truncate(fmt.Sprintf("#%d %s", pr.Number, pr.Title), 256), URL: pr.HTMLURL,
		Description: desc, Color: color, Button: button}
	return m, parts[1:], urls
}

// download fetches the files a description shows, to attach them: those
// too big for Discord are left out.
func (s *Service) download(urls []string) []discord.File {
	hc := &http.Client{Timeout: time.Minute}
	var out []discord.File
	for i, u := range urls {
		resp, err := hc.Get(u)
		if err != nil {
			s.log.Printf("discord: %s: %v", u, err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSize+1))
		resp.Body.Close()
		if err != nil || resp.StatusCode >= 300 || len(data) > maxFileSize {
			s.log.Printf("discord: %s: %s, %d bytes; not attached", u, resp.Status, len(data))
			continue
		}
		ct := resp.Header.Get("Content-Type")
		ext := ""
		for e, t := range github.ContentTypes {
			if strings.HasPrefix(ct, t) {
				ext = e
			}
		}
		if ext == "" {
			ext = path.Ext(u)
		}
		out = append(out, discord.File{Name: fmt.Sprintf("%d%s", i+1, ext), ContentType: ct, Data: data})
	}
	return out
}

// digest posts the morning's digest in #daily-digest, once a day after its
// hour: what merged since the last one, what waits on the owner, and the
// pull requests people outside opened. A day with none of them has none.
func (s *Service) digest(p *proj, posts map[int]*post, byPR map[int]*store.Thread) error {
	now := time.Now()
	today := now.Format("2006-01-02")
	if now.Hour() < s.cfg.Discord.DigestHour {
		return nil
	}
	if last, err := s.st.Get(digestKey); err != nil || last == today {
		return err
	}
	since, err := s.chat.Last(digestChannel)
	if err != nil {
		return err
	}
	if since.IsZero() {
		since = now.Add(-24 * time.Hour)
	}

	closed, err := s.gh.PRs(p.cfg.Repo, "closed")
	if err != nil {
		return err
	}
	var merged []string
	for _, pr := range closed {
		if pr.MergedAt != nil && pr.MergedAt.After(since) {
			line := fmt.Sprintf("- [#%d %s](%s)", pr.Number, pr.Title, pr.HTMLURL)
			if po := posts[pr.Number]; po != nil && po.Status != "" {
				line += " · " + po.Status
			}
			merged = append(merged, line)
		}
	}
	open, err := s.gh.PRs(p.cfg.Repo, "open")
	if err != nil {
		return err
	}
	var waiting []*github.PR
	var outside []string
	for i := range open {
		pr := &open[i]
		if t := byPR[pr.Number]; t != nil {
			if pr.Draft {
				continue
			}
			reviews, err := s.gh.Reviews(p.cfg.Repo, pr.Number)
			if err != nil {
				return err
			}
			if len(approvals(reviews, p.repo.Trusted)) == 0 {
				waiting = append(waiting, pr)
			}
		} else if !p.repo.Trusted(pr.User.Login) && pr.User.Type != "Bot" && pr.CreatedAt.After(since) {
			outside = append(outside, fmt.Sprintf("- [#%d %s](%s) by %s", pr.Number, pr.Title, pr.HTMLURL, pr.User.Login))
		}
	}
	if err := s.st.Set(digestKey, today); err != nil {
		return err
	}
	if len(merged)+len(waiting)+len(outside) == 0 {
		return nil
	}
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].CreatedAt.Before(waiting[j].CreatedAt) })

	title := fmt.Sprintf("📰 %s: %d merged, %d waiting on you", now.Format("Mon Jan 2"), len(merged), len(waiting))
	var desc []string
	if len(merged) > 0 {
		desc = append(desc, "**Merged**\n"+strings.Join(merged, "\n"))
	}
	if len(outside) > 0 {
		desc = append(desc, "**Opened by people outside**\n"+strings.Join(outside, "\n"))
	}
	if len(waiting) > 0 {
		desc = append(desc, "**Waiting on you** is in the thread, oldest first.")
	}
	parts := discord.Split(strings.Join(desc, "\n\n"), discord.Limit, 1900)
	id, err := s.chat.Post(digestChannel, discord.Message{Title: title, Description: parts[0], Color: blue})
	if err != nil {
		return err
	}
	thread, err := s.chat.Thread(digestChannel, id, title)
	if err != nil {
		return err
	}
	for _, part := range parts[1:] {
		if err := s.chat.Say(thread, part); err != nil {
			return err
		}
	}
	for _, pr := range waiting {
		m, _, urls := message(pr, "🟡 Ready for your review", yellow, &discord.Button{ID: fmt.Sprintf("approve:%d", pr.Number), Label: "Approve"})
		m.Files = s.download(urls)
		if err := s.chat.SayMessage(thread, m); err != nil {
			return err
		}
	}
	s.log.Printf("discord: posted the digest: %d merged, %d waiting, %d outside", len(merged), len(waiting), len(outside))
	return nil
}

func hash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", h[:8])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
