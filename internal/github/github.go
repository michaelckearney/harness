// Package github is the little of GitHub's API harness serve uses.
package github

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Client calls GitHub as a GitHub App, or with a token from a file.
type Client struct {
	API       string
	AppID     int64
	KeyFile   string
	TokenFile string
	HTTP      *http.Client

	mu     sync.Mutex
	tokens map[string]token // by owner
}

type token struct {
	value   string
	expires time.Time
}

type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type Issue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	BodyHTML    string    `json:"body_html"`
	State       string    `json:"state"`
	User        User      `json:"user"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"`
}

type Comment struct {
	ID        int64     `json:"id"`
	User      User      `json:"user"`
	Body      string    `json:"body"`
	BodyHTML  string    `json:"body_html"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

type PR struct {
	Number   int        `json:"number"`
	NodeID   string     `json:"node_id"`
	State    string     `json:"state"`
	Title    string     `json:"title"`
	Draft    bool       `json:"draft"`
	Body     string     `json:"body"`
	MergedAt *time.Time `json:"merged_at"`
	HTMLURL  string     `json:"html_url"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

type Review struct {
	ID          int64     `json:"id"`
	User        User      `json:"user"`
	Body        string    `json:"body"`
	BodyHTML    string    `json:"body_html"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	SubmittedAt time.Time `json:"submitted_at"`
}

type ReviewComment struct {
	ID        int64     `json:"id"`
	ReviewID  int64     `json:"pull_request_review_id"`
	User      User      `json:"user"`
	Body      string    `json:"body"`
	BodyHTML  string    `json:"body_html"`
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// Error is a response GitHub refused.
type Error struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

// Token returns a token for the repository's owner, for git as well as the
// API.
func (c *Client) Token(repo string) (string, error) {
	if c.TokenFile != "" {
		b, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	owner := strings.SplitN(repo, "/", 2)[0]
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tokens[owner]; ok && time.Until(t.expires) > 10*time.Minute {
		return t.value, nil
	}
	jwt, err := c.jwt()
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := c.do("GET", "/repos/"+repo+"/installation", "Bearer "+jwt, nil, &inst); err != nil {
		return "", fmt.Errorf("the GitHub App is not installed on %s: %w", repo, err)
	}
	var t struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.do("POST", fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), "Bearer "+jwt, nil, &t); err != nil {
		return "", err
	}
	if c.tokens == nil {
		c.tokens = map[string]token{}
	}
	c.tokens[owner] = token{t.Token, t.ExpiresAt}
	return t.Token, nil
}

func (c *Client) jwt() (string, error) {
	data, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("%s: not a PEM private key", c.KeyFile)
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, _ = k.(*rsa.PrivateKey)
	}
	if key == nil {
		return "", fmt.Errorf("%s: not an RSA private key", c.KeyFile)
	}
	now := time.Now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	// A minute back, for a clock a little ahead of GitHub's.
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": fmt.Sprint(c.AppID)})
	signed := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

func (c *Client) do(method, path, auth string, in, out any) error {
	_, err := c.request(method, c.url(path), auth, in, out)
	return err
}

func (c *Client) url(path string) string {
	if strings.HasPrefix(path, "http") {
		return path
	}
	return c.API + path
}

func (c *Client) request(method, u, auth string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	// full+json adds body_html, which in a private repository is the only
	// place a link to an attached image works without GitHub credentials.
	req.Header.Set("Accept", "application/vnd.github.full+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", auth)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		path := strings.TrimPrefix(u, c.API)
		return nil, &Error{Status: resp.StatusCode, Method: method, Path: path, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("github: %s %s: %w", method, u, err)
		}
	}
	return resp.Header, nil
}

// call makes a request as the repository's installation.
func (c *Client) call(repo, method, path string, in, out any) error {
	t, err := c.Token(repo)
	if err != nil {
		return err
	}
	return c.do(method, path, "token "+t, in, out)
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// list gets every page of a list.
func list[T any](c *Client, repo, path string) ([]T, error) {
	t, err := c.Token(repo)
	if err != nil {
		return nil, err
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u := c.url(path + sep + "per_page=100")
	var all []T
	for u != "" {
		var page []T
		h, err := c.request("GET", u, "token "+t, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		u = ""
		if m := nextLink.FindStringSubmatch(h.Get("Link")); m != nil {
			u = m[1]
		}
	}
	return all, nil
}

func (c *Client) DefaultBranch(repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := c.call(repo, "GET", "/repos/"+repo, nil, &r)
	return r.DefaultBranch, err
}

// Issues lists the issues, not pull requests, updated since a time.
func (c *Client) Issues(repo string, since time.Time) ([]Issue, error) {
	all, err := list[Issue](c, repo, "/repos/"+repo+"/issues?state=all&sort=updated&since="+url.QueryEscape(since.UTC().Format(time.RFC3339)))
	var out []Issue
	for _, i := range all {
		if i.PullRequest == nil {
			out = append(out, i)
		}
	}
	return out, err
}

func (c *Client) Issue(repo string, n int) (*Issue, error) {
	var i Issue
	return &i, c.call(repo, "GET", fmt.Sprintf("/repos/%s/issues/%d", repo, n), nil, &i)
}

// Comments lists the comments on an issue, or on a pull request's
// conversation.
func (c *Client) Comments(repo string, n int) ([]Comment, error) {
	return list[Comment](c, repo, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, n))
}

func (c *Client) Comment(repo string, n int, body string) error {
	return c.call(repo, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", repo, n), map[string]string{"body": body}, nil)
}

// SetBody replaces a pull request's description.
func (c *Client) SetBody(repo string, n int, body string) error {
	return c.call(repo, "PATCH", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), map[string]string{"body": body}, nil)
}

// SetTitle replaces a pull request's title.
func (c *Client) SetTitle(repo string, n int, title string) error {
	return c.call(repo, "PATCH", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), map[string]string{"title": title}, nil)
}

// Upload is a comment or a pull request description with files in it.
type Upload struct {
	Gh    string   // the gh command, logged in as the owner
	PR    bool     // the description of pull request N, not a comment on issue N
	N     int      // the issue or pull request
	Body  string   // Markdown that names each file, as an image, by the path given in Files
	Files []string // absolute paths
}

// Upload uploads each file, puts its URL where the body names it, and posts
// the body as the App.
//
// The files go up with the owner's gh login, not the App's token: GitHub's
// upload endpoint answers 404 to every App installation token, whatever its
// permissions (cli/cli#14309), and gh reports that as "attaching files
// requires write access to the repository". That refusal sent Harbor's bot
// to commit its recording into the branch. The owner's token never reaches
// the bot; it uploads only files harness serve has checked are in the worktree.
func (c *Client) Upload(repo string, u Upload) error {
	out, err := exec.Command(u.Gh, "auth", "token").Output()
	if err != nil {
		return fmt.Errorf("the owner's gh login, which uploads files: %w", err)
	}
	owner := strings.TrimSpace(string(out))
	var r struct {
		ID int64 `json:"id"`
	}
	if err := c.call(repo, "GET", "/repos/"+repo, nil, &r); err != nil {
		return err
	}
	body := u.Body
	for _, f := range u.Files {
		asset, err := c.uploadFile(owner, r.ID, f)
		if err != nil {
			return fmt.Errorf("could not upload %s: %w", f, err)
		}
		body = linkFile(body, f, asset)
	}
	if u.PR {
		return c.SetBody(repo, u.N, body)
	}
	return c.Comment(repo, u.N, body)
}

// contentTypes are the files GitHub takes as attachments.
var contentTypes = map[string]string{
	".png": "image/png", ".gif": "image/gif", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".svg": "image/svg+xml", ".webp": "image/webp",
	".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
}

// uploadFile sends one file to the endpoint gh's --attach uses and returns
// the URL to show it by.
func (c *Client) uploadFile(auth string, repoID int64, file string) (string, error) {
	ct := contentTypes[strings.ToLower(filepath.Ext(file))]
	if ct == "" {
		return "", fmt.Errorf("GitHub does not take %s files", filepath.Ext(file))
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	q := url.Values{"name": {filepath.Base(file)}, "content_type": {ct}, "repository_id": {fmt.Sprint(repoID)}}
	req, err := http.NewRequest("POST", c.uploads()+"/user-attachments/assets?"+q.Encode(), f)
	if err != nil {
		return "", err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "token "+auth)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub answered %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &a); err != nil || a.URL == "" {
		return "", fmt.Errorf("GitHub returned no URL: %s", strings.TrimSpace(string(data)))
	}
	return a.URL, nil
}

// uploads is the upload host beside the API: uploads.github.com for
// api.github.com. Any other API, such as a test's, takes uploads itself.
func (c *Client) uploads() string {
	if u, err := url.Parse(c.API); err == nil && strings.HasPrefix(u.Host, "api.") {
		u.Host = "uploads." + strings.TrimPrefix(u.Host, "api.")
		u.Path = ""
		return u.String()
	}
	return c.API
}

// linkFile puts an uploaded file's URL where the body names its path. A
// video becomes the bare URL, which GitHub plays when it is alone on its
// line; as an image it would be a broken picture.
func linkFile(body, file, asset string) string {
	ref := regexp.MustCompile(`!\[([^\]]*)\]\(\s*<?` + regexp.QuoteMeta(file) + `>?(\s+"[^"]*")?\s*\)`)
	video := strings.HasPrefix(contentTypes[strings.ToLower(filepath.Ext(file))], "video/")
	return ref.ReplaceAllStringFunc(body, func(m string) string {
		if video {
			return asset
		}
		sub := ref.FindStringSubmatch(m)
		return "![" + sub[1] + "](" + asset + sub[2] + ")"
	})
}

// FindPR returns the newest pull request from a branch, open or not, or nil.
func (c *Client) FindPR(repo, branch string) (*PR, error) {
	owner := strings.SplitN(repo, "/", 2)[0]
	prs, err := list[PR](c, repo, "/repos/"+repo+"/pulls?state=all&head="+url.QueryEscape(owner+":"+branch))
	if err != nil || len(prs) == 0 {
		return nil, err
	}
	return &prs[0], nil
}

func (c *Client) PR(repo string, n int) (*PR, error) {
	var p PR
	return &p, c.call(repo, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &p)
}

func (c *Client) CreatePR(repo, title, body, head, base string, draft bool) (*PR, error) {
	var p PR
	in := map[string]any{"title": title, "body": body, "head": head, "base": base, "draft": draft}
	return &p, c.call(repo, "POST", "/repos/"+repo+"/pulls", in, &p)
}

func (c *Client) Reviews(repo string, pr int) ([]Review, error) {
	return list[Review](c, repo, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr))
}

func (c *Client) ReviewComments(repo string, pr int) ([]ReviewComment, error) {
	return list[ReviewComment](c, repo, fmt.Sprintf("/repos/%s/pulls/%d/comments", repo, pr))
}

func (c *Client) CheckRuns(repo, sha string) ([]CheckRun, error) {
	var r struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	err := c.call(repo, "GET", fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", repo, sha), nil, &r)
	return Latest(r.CheckRuns), err
}

// JobLog is the log of a GitHub Actions job, by the ID of its check run,
// which is the job's. The App needs read access to Actions.
func (c *Client) JobLog(repo string, id int64) (string, error) {
	t, err := c.Token(repo)
	if err != nil {
		return "", err
	}
	u := c.url(fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", repo, id))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+t)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	// GitHub answers with a redirect to the log, which the client follows
	// without the token.
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", &Error{Status: resp.StatusCode, Method: "GET", Path: strings.TrimPrefix(u, c.API), Body: string(data)}
	}
	return string(data), nil
}

// Latest keeps the latest run of each check, by name: a check that ran
// again, as one does when a pull request is edited or a run is retried,
// is only what its last run says.
func Latest(runs []CheckRun) []CheckRun {
	at := map[string]int{}
	var out []CheckRun
	for _, r := range runs {
		i, seen := at[r.Name]
		switch {
		case !seen:
			at[r.Name] = len(out)
			out = append(out, r)
		case r.ID > out[i].ID:
			out[i] = r
		}
	}
	return out
}

// SetDraft turns a pull request into a draft, or marks it ready for review.
// Only GraphQL can do either.
func (c *Client) SetDraft(repo, nodeID string, draft bool) error {
	mutation := `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { clientMutationId } }`
	if draft {
		mutation = `mutation($id: ID!) { convertPullRequestToDraft(input: {pullRequestId: $id}) { clientMutationId } }`
	}
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	in := map[string]any{"query": mutation, "variables": map[string]string{"id": nodeID}}
	if err := c.call(repo, "POST", "/graphql", in, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("github: %s", out.Errors[0].Message)
	}
	return nil
}

func (c *Client) DeleteBranch(repo, branch string) error {
	err := c.call(repo, "DELETE", "/repos/"+repo+"/git/refs/heads/"+branch, nil, nil)
	if e, ok := err.(*Error); ok && (e.Status == 404 || e.Status == 422) {
		return nil
	}
	return err
}

// IssueEvent is one entry in an issue's timeline of events, such as a
// label being added.
type IssueEvent struct {
	Event     string    `json:"event"`
	Actor     User      `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
	Label     *struct {
		Name string `json:"name"`
	} `json:"label"`
}

// IssueEvents are an issue's events, oldest first.
func (c *Client) IssueEvents(repo string, n int) ([]IssueEvent, error) {
	return list[IssueEvent](c, repo, fmt.Sprintf("/repos/%s/issues/%d/events", repo, n))
}

// LabelsBy are the labels on an issue that someone trusted put there: the
// labels added by a trusted login and not removed since. A label anyone
// else adds says nothing, since a label can choose an agent that costs
// money or sends the code to a third party.
func (c *Client) LabelsBy(repo string, n int, trusted func(login string) bool) ([]string, error) {
	events, err := c.IssueEvents(repo, n)
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, e := range events {
		if e.Label == nil {
			continue
		}
		name := e.Label.Name
		switch {
		case e.Event == "labeled" && trusted(e.Actor.Login):
			labels = append(slices.DeleteFunc(labels, func(l string) bool { return l == name }), name)
		case e.Event == "unlabeled":
			labels = slices.DeleteFunc(labels, func(l string) bool { return l == name })
		}
	}
	return labels, nil
}
