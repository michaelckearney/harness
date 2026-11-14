package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/michaelckearney/harness/internal/host"
	"github.com/michaelckearney/harness/internal/service"
)

type fakeService struct {
	stopped, retried []string
	lines            chan service.Line
}

func (f *fakeService) Projects() ([]service.ProjectView, error) {
	return []service.ProjectView{{Repo: "o/r", Base: "main", Runner: "opencode", Ready: true, Threads: 1, Running: 1}}, nil
}
func (f *fakeService) Threads(repo string) ([]service.ThreadView, error) {
	return []service.ThreadView{{Repo: "o/r", Number: 7, State: "running"}}, nil
}
func (f *fakeService) Thread(repo string, n int) (service.ThreadView, error) {
	if repo != "o/r" || n != 7 {
		return service.ThreadView{}, service.ErrNoThread
	}
	return service.ThreadView{Repo: repo, Number: n, State: "running"}, nil
}
func (f *fakeService) Lines(repo string, n int) []service.Line {
	return []service.Line{{Repo: repo, Number: n, Kind: "text", Text: "hello"}}
}
func (f *fakeService) Subscribe() (<-chan service.Line, func()) { return f.lines, func() {} }
func (f *fakeService) Log(repo string, n int, max int64) (string, error) {
	return "=== the log\n", nil
}
func (f *fakeService) Stop(repo string, n int) error {
	f.stopped = append(f.stopped, repo)
	return nil
}
func (f *fakeService) Retry(repo string, n int) error {
	if n != 7 {
		return errors.New("o/r#8 is running a turn already")
	}
	f.retried = append(f.retried, repo)
	return nil
}

func setup(t *testing.T, login string) (*fakeService, *httptest.Server) {
	f := &fakeService{lines: make(chan service.Line, 4)}
	whois := func(ctx context.Context, addr string) (string, error) { return login, nil }
	srv := httptest.NewServer(New(f, host.API{Allow: []string{"Owner@example.com"}}, "v1.2.3", whois, log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	return f, srv
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestOnlyTheAllowedLoginIsAnswered(t *testing.T) {
	_, srv := setup(t, "stranger@example.com")
	if code, _ := get(t, srv.URL+"/api/v1/projects"); code != http.StatusForbidden {
		t.Errorf("a stranger got %d", code)
	}
	_, srv = setup(t, "owner@example.com")
	if code, body := get(t, srv.URL+"/api/v1/version"); code != 200 || !strings.Contains(body, `"api":1`) || !strings.Contains(body, "v1.2.3") {
		t.Errorf("version: %d %s", code, body)
	}
}

func TestReadsAndControls(t *testing.T) {
	f, srv := setup(t, "owner@example.com")
	if code, body := get(t, srv.URL+"/api/v1/projects"); code != 200 || !strings.Contains(body, `"repo":"o/r"`) {
		t.Errorf("projects: %d %s", code, body)
	}
	code, body := get(t, srv.URL+"/api/v1/threads/o/r/7")
	var v struct {
		Thread service.ThreadView
		Lines  []service.Line
	}
	if json.Unmarshal([]byte(body), &v); code != 200 || v.Thread.Number != 7 || len(v.Lines) != 1 {
		t.Errorf("thread: %d %s", code, body)
	}
	if code, _ := get(t, srv.URL+"/api/v1/threads/o/r/9"); code != 404 {
		t.Errorf("a thread that is not open: %d", code)
	}
	if code, body := get(t, srv.URL+"/api/v1/threads/o/r/7/log"); code != 200 || body != "=== the log\n" {
		t.Errorf("log: %d %q", code, body)
	}
	if code, _ := get(t, srv.URL+"/api/v1/threads/o/r/7/stop"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET stop: %d", code)
	}
	resp, _ := http.Post(srv.URL+"/api/v1/threads/o/r/7/stop", "", nil)
	resp2, _ := http.Post(srv.URL+"/api/v1/threads/o/r/8/retry", "", nil)
	if resp.StatusCode != 200 || len(f.stopped) != 1 || resp2.StatusCode != http.StatusConflict {
		t.Errorf("stop %d %v, retry of a running thread %d", resp.StatusCode, f.stopped, resp2.StatusCode)
	}
}

func TestLiveStreamsOneThread(t *testing.T) {
	f, srv := setup(t, "owner@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/live?repo=o/r&number=7", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	f.lines <- service.Line{Repo: "o/r", Number: 8, Text: "another thread"}
	f.lines <- service.Line{Repo: "o/r", Number: 7, Text: "this one"}
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, "this one") {
		n, err := resp.Body.Read(buf)
		if err != nil {
			t.Fatalf("%v after %q", err, got)
		}
		got += string(buf[:n])
	}
	if strings.Contains(got, "another thread") {
		t.Errorf("the stream for #7 showed #8: %q", got)
	}
}
