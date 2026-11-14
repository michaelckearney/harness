package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpenCode is `opencode serve`, as much of it as the service uses. The
// test binary runs it when HARNESS_FAKE_OPENCODE is set. A prompt runs the
// fake bot's scenario in the session's directory.
func fakeOpenCode() {
	port := ""
	for i, a := range os.Args {
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	var mu sync.Mutex
	sessions := map[string]bool{}
	subs := map[chan string]bool{}
	publish := func(e map[string]any) {
		b, _ := json.Marshal(e)
		mu.Lock()
		for c := range subs {
			c <- string(b)
		}
		mu.Unlock()
	}
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if _, pw, ok := r.BasicAuth(); !ok || pw != os.Getenv("OPENCODE_SERVER_PASSWORD") {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	http.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		if authed(w, r) {
			fmt.Fprint(w, `{"healthy":true,"version":"fake"}`)
		}
	})
	http.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		c := make(chan string, 64)
		mu.Lock()
		subs[c] = true
		mu.Unlock()
		defer func() { mu.Lock(); delete(subs, c); mu.Unlock() }()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-c:
				fmt.Fprintf(w, "data: %s\n\n", e)
				w.(http.Flusher).Flush()
			}
		}
	})
	http.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var body struct {
			Title      string `json:"title"`
			Permission []map[string]string
		}
		json.NewDecoder(r.Body).Decode(&body)
		perms, _ := json.Marshal(body.Permission)
		appendFile("sessions", body.Title+" "+r.URL.Query().Get("directory")+" "+string(perms)+"\n")
		mu.Lock()
		id := fmt.Sprintf("ses_%d", len(sessions)+1)
		sessions[id] = true
		mu.Unlock()
		fmt.Fprintf(w, `{"id":%q}`, id)
	})
	http.HandleFunc("GET /session/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := sessions[r.PathValue("id")]
		mu.Unlock()
		if !authed(w, r) {
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	http.HandleFunc("POST /session/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var body struct {
			Model  map[string]string
			System string
			Parts  []struct{ Text string }
		}
		json.NewDecoder(r.Body).Decode(&body)
		id := r.PathValue("id")
		appendFile("models", body.Model["providerID"]+"/"+body.Model["modelID"]+"\n")
		appendFile("system", body.System+"\n---\n")
		dir := r.URL.Query().Get("directory")
		appendFile("bot-env", "HARNESS_TEST_ENV="+os.Getenv("HARNESS_TEST_ENV")+"\n")
		cmd := exec.Command(filepath.Join(os.Getenv("FAKE_DIR"), "bot"), "--in", dir, "-z", body.Parts[0].Text)
		cmd.Run()
		publish(map[string]any{"type": "message.updated", "properties": map[string]any{"info": map[string]any{"id": "msg_1", "sessionID": id, "role": "assistant"}}})
		publish(map[string]any{"type": "message.part.updated", "properties": map[string]any{"part": map[string]any{
			"id": "prt_1", "sessionID": id, "messageID": "msg_1", "type": "text", "text": "I did the work.", "time": map[string]any{"start": 1, "end": 2}}}})
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, `{"info":{},"parts":[]}`)
	})
	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
		os.Exit(1)
	}
}

func appendFile(name, text string) {
	f, err := os.OpenFile(filepath.Join(os.Getenv("FAKE_DIR"), name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		f.WriteString(text)
		f.Close()
	}
}

func TestOpenCodeTakesTheTurns(t *testing.T) {
	h := setup(t)
	h.step()

	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step()

	dir := filepath.Join(h.s.cfg.StateDir, "agents", "opencode")
	data, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["model"] != "harness/test-model" || config["autoupdate"] != false {
		t.Errorf("opencode.json: %s", data)
	}
	if !strings.Contains(string(data), `"image"`) {
		t.Errorf("a model that can see is not told so: %s", data)
	}
	perm, _ := json.Marshal(config["permission"])
	for _, want := range []string{`"bash":{"*":"deny","harness":"allow","harness *":"allow"}`, `"question":"deny"`} {
		if !strings.Contains(string(perm), want) {
			t.Errorf("opencode.json's permission lacks %s: %s", want, perm)
		}
	}
	// The model is reached through the proxy, with its token, never a key.
	if !strings.Contains(string(data), h.s.proxy.URL+"/model") || !strings.Contains(string(data), h.s.proxy.Token) {
		t.Errorf("opencode does not go through the proxy: %s", data)
	}
	if sys := h.read("system"); !strings.Contains(sys, "# Working as a harness bot") || !strings.Contains(sys, "You work on the framework") {
		t.Errorf("the harness's instructions are not the bot's:\n%s", sys)
	}

	th := h.thread(n)
	if th.Session != "ses_1" || th.State != "idle" {
		t.Errorf("thread after the turn: %+v", th)
	}
	if got := h.read("sessions"); !strings.HasPrefix(got, fmt.Sprintf("gh-%d %s ", n, h.s.worktree(h.s.projects[0], n))) ||
		!strings.Contains(got, `{"action":"deny","pattern":"*","permission":"question"}`) ||
		!strings.Contains(got, `{"action":"allow","pattern":"harness *","permission":"bash"}`) {
		t.Errorf("sessions created: %q", got)
	}
	if got := h.read("models"); got != "harness/test-model\n" {
		t.Errorf("the prompt named %q", got)
	}
	if got := h.read("bot-env"); got != "HARNESS_TEST_ENV=from-the-host\nGH_TOKEN=\n" {
		t.Errorf("the bot saw %q", got)
	}
	if !strings.Contains(h.read("out"), "Committed") && len(h.gh.prs) == 0 {
		t.Errorf("the bot's commands did not reach the service:\n%s", h.read("out"))
	}
	lines := h.s.live.lines("o/r", n)
	var said bool
	for _, l := range lines {
		said = said || (l.Kind == "text" && l.Text == "I did the work.")
	}
	if !said {
		t.Errorf("the live view lacks the bot's text: %+v", lines)
	}

	// The next turn resumes the session.
	h.gh.comment(n, "owner", "Make it say hello world.")
	write(t, filepath.Join(h.fake, "scenario"), "more")
	h.step()
	if got := strings.Count(h.read("sessions"), "\n"); got != 1 {
		t.Errorf("%d sessions created, want 1", got)
	}
}

func TestTheOwnerCanRetryABlockedThread(t *testing.T) {
	h := setup(t)
	h.step()
	write(t, filepath.Join(h.fake, "scenario"), "silent")
	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step()
	if th := h.thread(n); th.State != "blocked" {
		t.Fatalf("a bot that never replies: %s", th.State)
	}
	if err := h.s.Stop("o/r", n); err == nil {
		t.Error("stopping a thread with no turn running succeeded")
	}

	write(t, filepath.Join(h.fake, "scenario"), "question")
	if err := h.s.Retry("o/r", n); err != nil {
		t.Fatal(err)
	}
	// What GitHub says must not drop the retry before a turn takes it.
	h.step()
	if th := h.thread(n); th.State != "idle" {
		t.Errorf("after the retry: %s", th.State)
	}
	if !strings.Contains(h.read("prompts"), "The owner asked you to try again") {
		t.Errorf("the retry did not reach the bot:\n%s", h.read("prompts"))
	}
	views, err := h.s.Threads("o/r")
	if err != nil || len(views) != 1 || views[0].Waiting != 0 {
		t.Errorf("threads after the retry: %+v %v", views, err)
	}
}
