package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTheKeyReplacesTheToken(t *testing.T) {
	var got http.Header
	var path string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("real-key\n"), 0o600)
	p, err := Start([]Route{{Name: "anthropic", BaseURL: up.URL, KeyFile: key, Beta: OAuthBeta}, {Name: "model", BaseURL: up.URL + "/v1", KeyFile: key}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	do := func(route, auth string) int {
		req, _ := http.NewRequest("POST", p.Base(route)+"/messages", nil)
		if auth != "" {
			req.Header.Set("x-api-key", auth)
		}
		req.Header.Set("anthropic-beta", "fine-grained-tool-streaming-2025-05-14")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := do("anthropic", "wrong"); code != 401 {
		t.Errorf("a wrong token: %d", code)
	}
	if code := do("anthropic", p.Token); code != 200 || got.Get("x-api-key") != "" || got.Get("Authorization") != "Bearer real-key" || path != "/messages" ||
		got.Get("anthropic-beta") != "fine-grained-tool-streaming-2025-05-14,"+OAuthBeta {
		t.Errorf("anthropic: %d %v %s", code, got, path)
	}
	req, _ := http.NewRequest("POST", p.Base("model")+"/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("Authorization") != "Bearer real-key" || path != "/v1/chat/completions" || got.Get("anthropic-beta") != "" {
		t.Errorf("model: %v %s", got, path)
	}
}

func TestALimitIsNoted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-reset", "4102444800")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer up.Close()
	p, err := Start([]Route{{Name: "anthropic", BaseURL: up.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Reset("anthropic").IsZero() {
		t.Fatal("a limit before any request")
	}
	req, _ := http.NewRequest("POST", p.Base("anthropic")+"/v1/messages", nil)
	req.Header.Set("x-api-key", p.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := p.Reset("anthropic").Unix(); got != 4102444800 {
		t.Errorf("reset at %d", got)
	}
}
