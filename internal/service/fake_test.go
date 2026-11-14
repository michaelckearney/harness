package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is as much of GitHub as harness serve uses, over one repository
// whose git side is a bare repository on disk.
type fakeGitHub struct {
	t      *testing.T
	repo   string // owner/name
	origin string // the bare repository

	mu       sync.Mutex
	next     int64
	issues   map[int]*fakeIssue
	comments map[int][]map[string]any
	prs      map[int]*fakePR
	reviews  map[int][]map[string]any
	checks   map[string][]map[string]any // by sha
	uploads  []string                    // "<auth> <name> <content type>"
	events   map[int][]map[string]any    // issue events, such as labels
}

type fakeIssue struct {
	Number  int
	Title   string
	Body    string
	State   string
	User    string
	Created time.Time
}

type fakePR struct {
	Number int
	Body   string
	Head   string
	Base   string
	Title  string
	Draft  bool
	State  string
	Merged bool
}

func newFakeGitHub(t *testing.T, repo, origin string) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{t: t, repo: repo, origin: origin, next: 100,
		issues: map[int]*fakeIssue{}, comments: map[int][]map[string]any{}, prs: map[int]*fakePR{},
		reviews: map[int][]map[string]any{}, checks: map[string][]map[string]any{}, events: map[int][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) id() int64 { f.next++; return f.next }

func (f *fakeGitHub) openIssue(user, title, body string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := int(f.id())
	f.issues[n] = &fakeIssue{Number: n, Title: title, Body: body, State: "open", User: user, Created: time.Now()}
	return n
}

// openScoped is an issue the owner labelled for the framework, which a bot
// works.
func (f *fakeGitHub) openScoped(user, title, body string) int {
	n := f.openIssue(user, title, body)
	f.label(n, user, "framework", true)
	return n
}

func (f *fakeGitHub) comment(n int, user, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id()
	f.comments[n] = append(f.comments[n], map[string]any{"id": id, "user": map[string]string{"login": user},
		"body": body, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"html_url": fmt.Sprintf("https://github.test/%s/pull/%d#issuecomment-%d", f.repo, n, id)})
}

// label has a user put a label on an issue, or take it off.
func (f *fakeGitHub) label(n int, user, name string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	event := "labeled"
	if !on {
		event = "unlabeled"
	}
	f.events[n] = append(f.events[n], map[string]any{"event": event, "actor": map[string]string{"login": user},
		"created_at": time.Now().UTC().Format(time.RFC3339Nano), "label": map[string]string{"name": name}})
}

func (f *fakeGitHub) commentsOn(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments[n] {
		out = append(out, c["body"].(string))
	}
	return out
}

func (f *fakeGitHub) pr(head string) *fakePR {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prs {
		if p.Head == head {
			return p
		}
	}
	return nil
}

func (f *fakeGitHub) sha(branch string) string {
	out, err := exec.Command("git", "-C", f.origin, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (f *fakeGitHub) issueJSON(i *fakeIssue) map[string]any {
	return map[string]any{"number": i.Number, "title": i.Title, "body": i.Body, "state": i.State,
		"user": map[string]string{"login": i.User}, "html_url": fmt.Sprintf("https://github.test/%s/issues/%d", f.repo, i.Number),
		"created_at": i.Created.UTC().Format(time.RFC3339Nano), "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
}

func (f *fakeGitHub) prJSON(p *fakePR) map[string]any {
	j := map[string]any{"number": p.Number, "node_id": fmt.Sprintf("PR_%d", p.Number), "state": p.State, "draft": p.Draft, "body": p.Body, "title": p.Title,
		"html_url": fmt.Sprintf("https://github.test/%s/pull/%d", f.repo, p.Number)}
	if p.Merged {
		j["merged_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	j["head"] = map[string]string{"ref": p.Head, "sha": f.sha(p.Head)}
	return j
}

var (
	reIssue    = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/(\d+)$`)
	reComments = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/(\d+)/comments$`)
	reEvents   = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/(\d+)/events$`)
	rePR       = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)$`)
	reReviews  = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)/reviews$`)
	reRC       = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)/comments$`)
	reChecks   = regexp.MustCompile(`^/repos/[^/]+/[^/]+/commits/([0-9a-f]+)/check-runs$`)
	reJobLog   = regexp.MustCompile(`^/repos/[^/]+/[^/]+/actions/jobs/(\d+)/logs$`)
	reRef      = regexp.MustCompile(`^/repos/[^/]+/[^/]+/git/refs/heads/(.+)$`)
)

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	num := func(re *regexp.Regexp) int { n, _ := strconv.Atoi(re.FindStringSubmatch(r.URL.Path)[1]); return n }
	var in map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&in)
	}
	p := r.URL.Path
	switch {
	case r.Method == "GET" && p == "/repos/"+f.repo:
		reply(map[string]any{"id": 42, "default_branch": "main"})
	case r.Method == "POST" && p == "/user-attachments/assets":
		q := r.URL.Query()
		if q.Get("repository_id") != "42" {
			http.NotFound(w, r)
			return
		}
		f.uploads = append(f.uploads, r.Header.Get("Authorization")+" "+q.Get("name")+" "+q.Get("content_type"))
		reply(map[string]string{"url": fmt.Sprintf("https://github.test/user-attachments/assets/%d", len(f.uploads))})
	case r.Method == "GET" && p == "/repos/"+f.repo+"/issues":
		var out []map[string]any
		for _, i := range f.issues {
			out = append(out, f.issueJSON(i))
		}
		reply(out)
	case r.Method == "GET" && reIssue.MatchString(p):
		i := f.issues[num(reIssue)]
		if i == nil {
			http.NotFound(w, r)
			return
		}
		reply(f.issueJSON(i))
	case r.Method == "GET" && reEvents.MatchString(p):
		out := f.events[num(reEvents)]
		if out == nil {
			out = []map[string]any{}
		}
		reply(out)
	case reComments.MatchString(p):
		n := num(reComments)
		if r.Method == "POST" {
			f.comments[n] = append(f.comments[n], map[string]any{"id": f.id(), "user": map[string]string{"login": "harness-bot[bot]"},
				"body": in["body"], "created_at": time.Now().UTC().Format(time.RFC3339Nano)})
			reply(map[string]any{})
			return
		}
		out := f.comments[n]
		if out == nil {
			out = []map[string]any{}
		}
		reply(out)
	case r.Method == "GET" && p == "/repos/"+f.repo+"/pulls":
		head := r.URL.Query().Get("head")
		out := []map[string]any{}
		for _, pr := range f.prs {
			if strings.HasSuffix(head, ":"+pr.Head) {
				out = append(out, f.prJSON(pr))
			}
		}
		reply(out)
	case r.Method == "POST" && p == "/repos/"+f.repo+"/pulls":
		n := int(f.id())
		pr := &fakePR{Number: n, Head: in["head"].(string), Base: in["base"].(string), Title: in["title"].(string),
			Draft: in["draft"].(bool), State: "open", Body: in["body"].(string)}
		f.prs[n] = pr
		reply(f.prJSON(pr))
	case r.Method == "PATCH" && rePR.MatchString(p):
		pr := f.prs[num(rePR)]
		if pr == nil {
			http.NotFound(w, r)
			return
		}
		if body, ok := in["body"].(string); ok {
			pr.Body = body
		}
		if title, ok := in["title"].(string); ok {
			pr.Title = title
		}
		reply(f.prJSON(pr))
	case r.Method == "GET" && rePR.MatchString(p):
		pr := f.prs[num(rePR)]
		if pr == nil {
			http.NotFound(w, r)
			return
		}
		reply(f.prJSON(pr))
	case r.Method == "GET" && reReviews.MatchString(p):
		out := f.reviews[num(reReviews)]
		if out == nil {
			out = []map[string]any{}
		}
		reply(out)
	case r.Method == "GET" && reRC.MatchString(p):
		reply([]any{})
	case r.Method == "GET" && reChecks.MatchString(p):
		runs := f.checks[reChecks.FindStringSubmatch(p)[1]]
		if runs == nil {
			runs = []map[string]any{}
		}
		reply(map[string]any{"check_runs": runs})
	case r.Method == "GET" && reJobLog.MatchString(p):
		// Job 4's log is there; any other job's, GitHub refuses, as it does
		// an App without read access to Actions.
		if num(reJobLog) != 4 {
			w.WriteHeader(403)
			reply(map[string]any{"message": "Resource not accessible by integration"})
			return
		}
		fmt.Fprint(w, "2026-11-11T03:57:05.0561416Z [go test]     hello_test.go:9: want hello world\n"+
			"2026-11-11T03:57:05.0563248Z [go test] --- FAIL: TestHello (0.62s)\n"+
			"2026-11-11T03:57:08.0515292Z \x1b[31mtest failed\x1b[0m\n"+
			"2026-11-11T03:57:08.0535196Z ##[error]Process completed with exit code 1.\n"+
			"2026-11-11T03:57:09.0000000Z Post job cleanup.\n")
	case r.Method == "POST" && p == "/graphql":
		q := in["query"].(string)
		id := in["variables"].(map[string]any)["id"].(string)
		n, _ := strconv.Atoi(strings.TrimPrefix(id, "PR_"))
		if pr := f.prs[n]; pr != nil {
			pr.Draft = strings.Contains(q, "convertPullRequestToDraft")
		}
		reply(map[string]any{"data": map[string]any{}})
	case r.Method == "DELETE" && reRef.MatchString(p):
		exec.Command("git", "-C", f.origin, "update-ref", "-d", "refs/heads/"+reRef.FindStringSubmatch(p)[1]).Run()
		w.WriteHeader(204)
	default:
		f.t.Errorf("fake GitHub: unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) uploaded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.uploads...)
}
