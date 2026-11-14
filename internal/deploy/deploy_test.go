package deploy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelckearney/harness/internal/host"
)

const serverYAML = `schema: 1
agents: [opencode, claude-code]
model: { base_url: "http://127.0.0.1:8000/v1", id: m }
github: { app_id: 7 }
api: { port: 7419, allow: [me@example.com] }
`

func loadServer(t *testing.T, data string) (*Server, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadServer(path)
}

// fakeBin writes a command into dir.
func fakeBin(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDeployInstallsTheService(t *testing.T) {
	srv, err := loadServer(t, serverYAML)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	fakes := filepath.Join(root, "bin")
	bins := filepath.Join(root, "binaries")
	for _, d := range []string{home, fakes, bins} {
		os.MkdirAll(d, 0o755)
	}
	// The server is here, in a home of its own, with a fake systemd.
	t.Setenv("HOME", home)
	t.Setenv("PATH", fakes+":"+os.Getenv("PATH"))
	fakeBin(t, fakes, "systemctl", `echo "systemctl $*" >> `+root+`/systemctl`)
	fakeBin(t, fakes, "loginctl", `exit 0`)
	fakeBin(t, fakes, "opencode", `exit 0`)
	fakeBin(t, fakes, "claude", `exit 0`)
	// The harness that is installed: one that validates and says its version.
	fakeBin(t, bins, "harness", `case "$1" in version) echo v9.9.9 ;; serve) cp "$4" `+root+`/validated ;; esac`)

	env := map[string]string{EnvAppKey: "-----BEGIN KEY-----", EnvClaudeToken: "sk-ant-oat"}
	opts := func(v string) Options {
		return Options{Repo: "o/poc", Exe: filepath.Join(bins, "harness"), Version: v,
			Author: host.Author{Name: "harness", Email: "1+o@users.noreply.github.com"}, Env: func(k string) string { return env[k] }}
	}
	var out bytes.Buffer
	err = Run(context.Background(), srv, opts("v9.9.9"), &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	conf := filepath.Join(home, ".config/harness/poc")
	hostYAML, _ := os.ReadFile(filepath.Join(conf, "host.yaml"))
	for _, want := range []string{"repo: o/poc", "base_url: http://127.0.0.1:8000/v1", "listen: tailnet:7419",
		"private_key: ~/.config/harness/poc/github-app.pem", "token_file: ~/.config/harness/poc/claude.token", "- claude-code",
		"email: 1+o@users.noreply.github.com"} {
		if !strings.Contains(string(hostYAML), want) {
			t.Errorf("host.yaml lacks %q:\n%s", want, hostYAML)
		}
	}
	if key, _ := os.ReadFile(filepath.Join(conf, "claude.token")); string(key) != "sk-ant-oat\n" {
		t.Errorf("claude.token: %q", key)
	}
	if info, err := os.Stat(filepath.Join(conf, "github-app.pem")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the App's key: %v %v", info, err)
	}
	unit, _ := os.ReadFile(filepath.Join(home, ".config/systemd/user/harness-poc.service"))
	for _, want := range []string{"ExecStart=" + home + "/.local/share/harness/poc/harness serve --config " + conf + "/host.yaml", "UMask=0007",
		"OnFailure=harness-poc-rollback.service", "RestartForceExitStatus=75"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	rollback, _ := os.ReadFile(filepath.Join(home, ".config/systemd/user/harness-poc-rollback.service"))
	if !strings.Contains(string(rollback), "ExecStart="+conf+"/rollback "+home+"/.local/share/harness/poc/harness harness-poc.service") {
		t.Errorf("rollback unit:\n%s", rollback)
	}
	if _, err := os.Stat(filepath.Join(root, "validated")); err != nil {
		t.Error("the configuration was not validated before the restart")
	}
	if calls, _ := os.ReadFile(filepath.Join(root, "systemctl")); !strings.Contains(string(calls), "restart --no-block harness-poc.service") {
		t.Errorf("systemctl: %s", calls)
	}

	// While the old service lets its turns finish, the deploy waits for it,
	// however long that is, rather than putting the previous one back.
	fakeBin(t, fakes, "systemctl", `echo "systemctl $*" >> `+root+`/systemctl
case "$*" in *ActiveState*)
	n=$(cat `+root+`/stops 2>/dev/null || echo 0); echo $((n + 1)) > `+root+`/stops
	[ "$n" -lt 4 ] && echo deactivating || echo active ;;
esac`)
	out.Reset()
	t.Setenv("DEPLOY_WAIT", "2")
	err = Run(context.Background(), srv, opts("v9.9.9"), &out)
	if err != nil || !strings.Contains(out.String(), "waiting for the running turns to finish") {
		t.Errorf("a deploy while turns finish: %v\n%s", err, out.String())
	}
	fakeBin(t, fakes, "systemctl", `echo "systemctl $*" >> `+root+`/systemctl`)

	// A version that does not come up is put back.
	fakeBin(t, bins, "harness", `case "$1" in version) echo v0.0.1 ;; esac`)
	out.Reset()
	err = Run(context.Background(), srv, opts("v10.0.0"), &out)
	if err == nil || !strings.Contains(out.String(), "putting the previous one back") {
		t.Errorf("a bad version: %v\n%s", err, out.String())
	}
	got, _ := exec.Command(filepath.Join(home, ".local/share/harness/poc/harness"), "version").Output()
	if strings.TrimSpace(string(got)) != "v9.9.9" {
		t.Errorf("after the failed deploy the server runs %q", got)
	}
}

func TestDeployNeedsTheSecrets(t *testing.T) {
	srv, err := loadServer(t, serverYAML)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Files("o/poc", srv, host.Author{}, func(k string) string { return map[string]string{EnvAppKey: "k"}[k] })
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("no Claude token: %v", err)
	}
}

func TestServerFile(t *testing.T) {
	for _, bad := range []struct{ change, want string }{
		{"agents: [opencode, claude-code]|agents: [opencode, pi]", "not an agent"},
		{"model: { base_url: \"http://127.0.0.1:8000/v1\", id: m }|model: { id: m }", "opencode needs base_url"},
		{"api: { port: 7419, allow: [me@example.com] }|api: { port: 7419 }", "api: give port and allow"},
		{"schema: 1|schema: 1\nhost: spark", "host"},
	} {
		from, to, _ := strings.Cut(bad.change, "|")
		if _, err := loadServer(t, strings.Replace(serverYAML, from, to, 1)); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: %v, want %q", bad.change, err, bad.want)
		}
	}
}

func TestRepoOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/owner/mono": "owner/mono", "https://github.com/owner/mono.git": "owner/mono", "https://github.com/owner/mono/": "owner/mono",
	} {
		if got, err := RepoOf(in); err != nil || got != want {
			t.Errorf("RepoOf(%s) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"owner/mono", "https://gitlab.com/owner/mono", "https://github.com/owner"} {
		if _, err := RepoOf(bad); err == nil {
			t.Errorf("RepoOf(%s): no error", bad)
		}
	}
}

func TestNoreply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/Owner" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id": 42, "login": "owner"}`)
	}))
	defer srv.Close()
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "https://api.github.com" })
	a, err := Noreply(context.Background(), "Owner")
	if err != nil || a.Name != "harness" || a.Email != "42+owner@users.noreply.github.com" {
		t.Errorf("Noreply: %+v %v", a, err)
	}
	if _, err := Noreply(context.Background(), "nobody"); err == nil {
		t.Error("no such user: no error")
	}
}
