// Package api is what the desktop app reads from `harness serve`: the
// projects, their threads, and what each bot is doing, live. It is
// read-only, apart from stopping and retrying a turn. It does not show GitHub:
// the app asks GitHub itself, with the owner's own gh.
//
// It listens only on the tailnet, and answers only the Tailscale logins the
// host allows, checked with `tailscale whois` on every connection.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/michaelckearney/harness/internal/host"
	"github.com/michaelckearney/harness/internal/service"
)

// Version is the API's version. The app refuses a server whose version it
// does not know, rather than misreading it.
const Version = 1

// Service is what the API needs of `harness serve`.
type Service interface {
	Projects() ([]service.ProjectView, error)
	Threads(repo string) ([]service.ThreadView, error)
	Thread(repo string, n int) (service.ThreadView, error)
	Lines(repo string, n int) []service.Line
	Subscribe() (<-chan service.Line, func())
	Log(repo string, n int, max int64) (string, error)
	Stop(repo string, n int) error
	Retry(repo string, n int) error
}

// Whois says which Tailscale login is at a remote address.
type Whois func(ctx context.Context, addr string) (string, error)

// Server is the API.
type Server struct {
	svc     Service
	allow   []string
	whois   Whois
	version string
	log     *log.Logger

	mu    sync.Mutex
	cache map[string]cached // by remote address
}

type cached struct {
	login string
	until time.Time
}

// New makes the API for a service. version is the harness's own.
func New(svc Service, cfg host.API, version string, whois Whois, logger *log.Logger) *Server {
	if whois == nil {
		whois = TailscaleWhois(cfg.Tailscale)
	}
	return &Server{svc: svc, allow: cfg.Allow, whois: whois, version: version, log: logger, cache: map[string]cached{}}
}

// Listen resolves the configured address: "tailnet" becomes this machine's
// Tailscale IPv4 address.
func Listen(cfg host.API) (net.Listener, error) {
	h, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("api.listen: %w", err)
	}
	if h == "tailnet" {
		out, err := exec.Command(cfg.Tailscale, "ip", "-4").Output()
		if err != nil {
			return nil, fmt.Errorf("api.listen: tailscale ip -4: %w", err)
		}
		h = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if h == "" {
			return nil, errors.New("api.listen: this machine has no Tailscale address; is it on the tailnet?")
		}
	}
	return net.Listen("tcp", net.JoinHostPort(h, port))
}

// TailscaleWhois asks tailscaled who is at an address.
func TailscaleWhois(command string) Whois {
	return func(ctx context.Context, addr string) (string, error) {
		out, err := exec.CommandContext(ctx, command, "whois", "--json", addr).Output()
		if err != nil {
			return "", fmt.Errorf("tailscale whois %s: %w", addr, err)
		}
		var w struct {
			UserProfile struct {
				LoginName string
			}
		}
		if err := json.Unmarshal(out, &w); err != nil {
			return "", err
		}
		return w.UserProfile.LoginName, nil
	}
}

// Serve answers on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	s.log.Printf("api: listening on %s for %s", ln.Addr(), strings.Join(s.allow, ", "))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler is the API's routes, behind the identity check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"api": Version, "harness": s.version})
	})
	mux.HandleFunc("GET /api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.svc.Projects()
		answer(w, v, err)
	})
	mux.HandleFunc("GET /api/v1/threads", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.svc.Threads(r.URL.Query().Get("repo"))
		answer(w, v, err)
	})
	mux.HandleFunc("GET /api/v1/threads/{owner}/{name}/{n}", func(w http.ResponseWriter, r *http.Request) {
		repo, n, ok := thread(w, r)
		if !ok {
			return
		}
		t, err := s.svc.Thread(repo, n)
		if err != nil {
			answer(w, nil, err)
			return
		}
		lines := s.svc.Lines(repo, n)
		if lines == nil {
			lines = []service.Line{}
		}
		reply(w, map[string]any{"thread": t, "lines": lines})
	})
	mux.HandleFunc("GET /api/v1/threads/{owner}/{name}/{n}/log", func(w http.ResponseWriter, r *http.Request) {
		repo, n, ok := thread(w, r)
		if !ok {
			return
		}
		text, err := s.svc.Log(repo, n, 4<<20)
		if err != nil {
			answer(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(text))
	})
	mux.HandleFunc("POST /api/v1/threads/{owner}/{name}/{n}/stop", func(w http.ResponseWriter, r *http.Request) {
		if repo, n, ok := thread(w, r); ok {
			answer(w, map[string]bool{"ok": true}, s.svc.Stop(repo, n))
		}
	})
	mux.HandleFunc("POST /api/v1/threads/{owner}/{name}/{n}/retry", func(w http.ResponseWriter, r *http.Request) {
		if repo, n, ok := thread(w, r); ok {
			answer(w, map[string]bool{"ok": true}, s.svc.Retry(repo, n))
		}
	})
	mux.HandleFunc("GET /api/v1/live", s.live)
	return s.check(mux)
}

// live streams lines as server-sent events, of one thread with ?repo= and
// ?number=, or of every thread.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	n, _ := strconv.Atoi(r.URL.Query().Get("number"))
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	lines, stop := s.svc.Subscribe()
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case l := <-lines:
			if (repo != "" && l.Repo != repo) || (n != 0 && l.Number != n) {
				continue
			}
			b, _ := json.Marshal(l)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// check answers only the allowed Tailscale logins.
func (s *Server) check(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, err := s.who(r.Context(), r.RemoteAddr)
		if err != nil {
			s.log.Printf("api: %s: %v", r.RemoteAddr, err)
			http.Error(w, "cannot tell who you are on the tailnet", http.StatusForbidden)
			return
		}
		if !slices.ContainsFunc(s.allow, func(a string) bool { return strings.EqualFold(a, login) }) {
			s.log.Printf("api: %s (%s) refused", r.RemoteAddr, login)
			http.Error(w, login+" may not use this API", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) who(ctx context.Context, addr string) (string, error) {
	s.mu.Lock()
	c, ok := s.cache[addr]
	s.mu.Unlock()
	if ok && time.Now().Before(c.until) {
		return c.login, nil
	}
	login, err := s.whois(ctx, addr)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.cache[addr] = cached{login: login, until: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	return login, nil
}

func thread(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		http.Error(w, "the thread number must be a positive integer", http.StatusBadRequest)
		return "", 0, false
	}
	return r.PathValue("owner") + "/" + r.PathValue("name"), n, true
}

func answer(w http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, service.ErrNoThread):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		reply(w, v)
	}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
