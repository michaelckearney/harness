// Package opencode runs `opencode serve` for one project and talks to it:
// sessions, prompts, and the stream of what the agent is doing.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Model is the model every prompt names, so a project's own opencode.json
// cannot choose another one.
type Model struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// Server is a running `opencode serve`.
type Server struct {
	URL      string
	password string
	cmd      *exec.Cmd
	done     chan struct{}
	client   *http.Client
}

// PasswordEnv carries the server's password to opencode.
const PasswordEnv = "OPENCODE_SERVER_PASSWORD"

// NewPassword is a fresh password for a server: the caller puts it in the
// server's environment as PasswordEnv, through whatever starts it, so that
// sudo keeps it.
func NewPassword() string {
	pw := make([]byte, 16)
	rand.Read(pw)
	return hex.EncodeToString(pw)
}

// Start runs `opencode serve` on a free loopback port and waits until it
// answers. argv is the command to run it with, such as ["opencode"], and env
// its whole environment, which sets PasswordEnv to password. Its output
// goes to log.
func Start(password string, argv []string, env []string, log io.Writer) (*Server, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	s := &Server{
		URL:      "http://127.0.0.1:" + strconv.Itoa(port),
		password: password,
		done:     make(chan struct{}),
		// No timeout: a prompt waits for the whole turn.
		client: &http.Client{},
	}
	args := append(append([]string{}, argv[1:]...), "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd := exec.Command(argv[0], args...)
	// Basic auth, so another user's process on this machine that finds the
	// port cannot drive the bots: env carries the password.
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = SysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.cmd = cmd
	go func() { cmd.Wait(); close(s.done) }()

	deadline := time.Now().Add(60 * time.Second)
	for {
		var h struct {
			Healthy bool `json:"healthy"`
		}
		// Each try has its own deadline: a server still starting can take
		// the connection and never answer it, which would wait forever.
		try, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.do(try, "GET", "/global/health", "", nil, &h)
		cancel()
		if err == nil && h.Healthy {
			return s, nil
		}
		select {
		case <-s.done:
			return nil, fmt.Errorf("opencode serve exited before it answered; see its log")
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			s.Stop()
			return nil, fmt.Errorf("opencode serve did not answer within a minute")
		}
	}
}

// Stop ends the server and everything it started.
func (s *Server) Stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
}

// Alive reports whether the server process is still running.
func (s *Server) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Rule is one entry of a session's permission ruleset.
type Rule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

// CreateSession starts a session in dir and returns its id.
func (s *Server) CreateSession(ctx context.Context, dir, title string, rules []Rule) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := s.do(ctx, "POST", "/session", dir, map[string]any{"title": title, "permission": rules}, &out)
	if err == nil && out.ID == "" {
		err = errors.New("opencode created a session with no id")
	}
	return out.ID, err
}

// ErrNoSession is returned for a session the server does not have.
var ErrNoSession = errors.New("no such session")

// Session checks that a session exists.
func (s *Server) Session(ctx context.Context, dir, id string) error {
	return s.do(ctx, "GET", "/session/"+url.PathEscape(id), dir, nil, nil)
}

// Prompt sends one message and waits until the agent has finished with it.
// Cancelling ctx aborts the session's work.
func (s *Server) Prompt(ctx context.Context, dir, id string, model Model, system, text string) error {
	var out struct {
		Info struct {
			Error *struct {
				Name string          `json:"name"`
				Data json.RawMessage `json:"data"`
			} `json:"error"`
		} `json:"info"`
	}
	body := map[string]any{
		"model": model,
		"parts": []map[string]any{{"type": "text", "text": text}},
	}
	if system != "" {
		body["system"] = system
	}
	err := s.do(ctx, "POST", "/session/"+url.PathEscape(id)+"/message", dir, body, &out)
	if ctx.Err() != nil {
		abort, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.do(abort, "POST", "/session/"+url.PathEscape(id)+"/abort", dir, nil, nil)
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if e := out.Info.Error; e != nil {
		return fmt.Errorf("opencode: %s: %s", e.Name, strings.TrimSpace(string(e.Data)))
	}
	return nil
}

// Event is one event from the server's stream.
type Event struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// Part is a message part, as message.part.updated carries it: the fields
// harness serve shows.
type Part struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	Tool      string `json:"tool"`
	State     struct {
		Status string          `json:"status"`
		Title  string          `json:"title"`
		Input  json.RawMessage `json:"input"`
		Output string          `json:"output"`
		Error  string          `json:"error"`
	} `json:"state"`
	Time struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	} `json:"time"`
}

// Events calls fn with each event for dir until ctx ends or the stream does.
func (s *Server) Events(ctx context.Context, dir string, fn func(Event)) error {
	req, err := s.request(ctx, "GET", "/event", dir, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode /event: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && data.Len() > 0:
			var e Event
			if json.Unmarshal(data.Bytes(), &e) == nil {
				fn(e)
			}
			data.Reset()
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

func (s *Server) request(ctx context.Context, method, path, dir string, body any) (*http.Request, error) {
	u := s.URL + path
	if dir != "" {
		u += "?directory=" + url.QueryEscape(dir)
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth("opencode", s.password)
	return req, nil
}

func (s *Server) do(ctx context.Context, method, path, dir string, body, out any) error {
	req, err := s.request(ctx, method, path, dir, body)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoSession
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("opencode %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
