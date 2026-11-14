// Package deploy sets up a monorepo's service on the server it runs on:
// the harness binary, the service's configuration and keys, the bots' user
// if there is one, and the systemd user units. What the service needs comes
// from the server's own file and secrets, never from the monorepo: a
// monorepo says only which harness it uses, and the service updates itself
// to that (see the service's update). Reaching the server, such as over SSH
// with Ansible, is whoever runs deploy's business.
//
// It owns only its own namespace there: the monorepo's binary,
// configuration, state, units, and bots' user. The machine itself, such as
// its packages and the model server, is someone else's.
//
// A service that does not come back healthy is put back as it was.
package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/michaelckearney/harness/internal/host"
	"github.com/michaelckearney/harness/internal/repo"
)

// The secrets deploy reads from its environment.
const (
	EnvAppKey      = "HARNESS_GITHUB_APP_KEY"  // the GitHub App's private key, PEM
	EnvToken       = "HARNESS_GITHUB_TOKEN"    // instead of an App, a token (for trying it out)
	EnvClaudeToken = "CLAUDE_CODE_OAUTH_TOKEN" // the Claude subscription's token, from claude setup-token
	EnvCodexAuth   = "CODEX_AUTH_JSON"         // the auth.json that codex login writes, for ChatGPT
	EnvModelKey    = "HARNESS_MODEL_API_KEY"   // the model endpoint's key, if it has one
	EnvOwnerGhAuth = "HARNESS_OWNER_GH_TOKEN"
)

// Server is the server's file: what the service for one monorepo needs that
// the monorepo does not say.
//
//	schema: 1
//	agents: [opencode, claude-code]
//	bot_user: harness-bot
//	model: { base_url: http://127.0.0.1:8000/v1, id: qwen3.6, vision: true }
//	github: { app_id: 123456 }
//	api: { port: 7420, allow: [me@example.com] }
type Server struct {
	Schema int `yaml:"schema"`
	// Agents are the agents this server runs: the threads whose agent is
	// another are another server's.
	Agents []string `yaml:"agents"`
	// Sessions is how many turns run at once. 1 by default.
	Sessions int `yaml:"sessions"`
	// BotUser, if set, is the user the agents run as: no sudo, no gh login,
	// no say over the service. Deploy makes it.
	BotUser string `yaml:"bot_user"`
	// Model is the OpenAI-compatible endpoint opencode uses.
	Model struct {
		BaseURL string `yaml:"base_url"`
		ID      string `yaml:"id"`
		Vision  bool   `yaml:"vision"`
		Context int    `yaml:"context"`
	} `yaml:"model"`
	// GitHub is the GitHub App the bots post as, whose key is EnvAppKey.
	GitHub struct {
		AppID int64 `yaml:"app_id"`
	} `yaml:"github"`
	// API serves the Slipway app, on the tailnet. Off without a port.
	API struct {
		Port int `yaml:"port"`
		// Allow are the Tailscale logins that may use it.
		Allow []string `yaml:"allow"`
	} `yaml:"api"`
}

var validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// LoadServer reads and checks a server's file.
func LoadServer(path string) (*Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Server
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func (s *Server) check() error {
	if s.Schema != 1 {
		return fmt.Errorf("schema must be 1")
	}
	if len(s.Agents) == 0 {
		return fmt.Errorf("agents: list the agents this server runs")
	}
	for _, a := range s.Agents {
		if !slices.Contains(repo.AllAgents, a) {
			return fmt.Errorf("agents: %q is not an agent; the agents are %s", a, strings.Join(repo.AllAgents, ", "))
		}
	}
	if s.Sessions == 0 {
		s.Sessions = 1
	}
	if s.Sessions < 0 {
		return fmt.Errorf("sessions must be 1 or more")
	}
	if s.BotUser != "" && !validUser.MatchString(s.BotUser) {
		return fmt.Errorf("bot_user %q is not a user name", s.BotUser)
	}
	if slices.Contains(s.Agents, repo.OpenCode) && (s.Model.BaseURL == "" || s.Model.ID == "") {
		return fmt.Errorf("model: opencode needs base_url and id, an OpenAI-compatible endpoint")
	}
	if s.API.Port < 0 || (s.API.Port > 0) != (len(s.API.Allow) > 0) {
		return fmt.Errorf("api: give port and allow, the Tailscale logins that may use it, or neither")
	}
	return nil
}

var githubURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(\.git)?/?$`)

// RepoOf is owner/name for a repository's URL on GitHub, such as
// https://github.com/owner/name.
func RepoOf(u string) (string, error) {
	m := githubURL.FindStringSubmatch(u)
	if m == nil {
		return "", fmt.Errorf("%q is not a repository on GitHub, such as https://github.com/owner/name", u)
	}
	return m[1], nil
}

// Options are what a deploy does.
type Options struct {
	// Repo is the monorepo, owner/name.
	Repo string
	// Exe is the harness binary to install.
	Exe string
	// Version is Exe's version, which the service must report.
	Version string
	// Author is who the bots' commits are by.
	Author host.Author
	// DryRun prints the script, secrets and all, instead of running it.
	DryRun bool
	// Env is where the secrets come from; os.Getenv by default.
	Env func(string) string
}

// Run sets up the monorepo's service on this server.
func Run(ctx context.Context, s *Server, o Options, out io.Writer) error {
	if o.Env == nil {
		o.Env = os.Getenv
	}
	inst := Instance(o.Repo)
	files, err := Files(o.Repo, s, o.Author, o.Env)
	if err != nil {
		return err
	}
	script, err := Script(inst, o.Repo, s, files, o.Version)
	if err != nil {
		return err
	}
	if o.DryRun {
		fmt.Fprintln(out, script)
		return nil
	}
	// The binary goes beside where it goes, for the script to move.
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	share := filepath.Join(home, ".local", "share", "harness", inst)
	if err := os.MkdirAll(share, 0o755); err != nil {
		return err
	}
	if err := copyFile(o.Exe, filepath.Join(share, "harness.new"), 0o755); err != nil {
		return fmt.Errorf("copying the binary: %w", err)
	}
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: running harness %s\n", o.Repo, o.Version)
	return nil
}

// Instance names one monorepo's service on a server.
func Instance(repoName string) string {
	return strings.ToLower(strings.SplitN(repoName, "/", 2)[1])
}

func copyFile(from, to string, mode os.FileMode) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := to + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

// githubAPI is GitHub's API; tests replace it.
var githubAPI = "https://api.github.com"

// Noreply is who the bots' commits are by: the harness, with the GitHub
// noreply address of the monorepo's owner, so their commits are the
// owner's on GitHub and say nothing about the machine they were made on.
func Noreply(ctx context.Context, owner string) (host.Author, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/users/"+url.PathEscape(owner), nil)
	if err != nil {
		return host.Author{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return host.Author{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return host.Author{}, fmt.Errorf("looking up %s on GitHub: %s", owner, resp.Status)
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return host.Author{}, err
	}
	if u.ID == 0 || u.Login == "" {
		return host.Author{}, fmt.Errorf("GitHub has no id for %s", owner)
	}
	return host.Author{Name: "harness", Email: fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)}, nil
}

// File is one file deploy writes on the server, under the instance's
// configuration folder.
type File struct {
	Name string
	Mode os.FileMode
	Data []byte
}

// Files are the service's configuration and keys.
func Files(repoName string, s *Server, author host.Author, env func(string) string) ([]File, error) {
	inst := Instance(repoName)
	dir := "~/.config/harness/" + inst
	hc := host.Config{
		Schema:   1,
		Name:     inst,
		Repo:     repoName,
		StateDir: "~/.local/state/harness/" + inst,
		Sessions: s.Sessions,
		Agents:   s.Agents,
		Author:   author,
	}
	if s.BotUser != "" {
		hc.User, hc.Group = s.BotUser, s.BotUser
		hc.StateDir = "/var/lib/harness-" + inst
	}
	var files []File
	secret := func(name, value string) string {
		files = append(files, File{Name: name, Mode: 0o600, Data: []byte(strings.TrimSpace(value) + "\n")})
		return dir + "/" + name
	}
	runs := func(a string) bool { return slices.Contains(s.Agents, a) }
	if runs(repo.OpenCode) {
		hc.Model = host.Model{BaseURL: s.Model.BaseURL, ID: s.Model.ID, Vision: s.Model.Vision, Context: s.Model.Context}
		if k := env(EnvModelKey); k != "" {
			hc.Model.KeyFile = secret("model.key", k)
		}
	}
	if runs(repo.ClaudeCode) {
		k := env(EnvClaudeToken)
		if k == "" {
			return nil, fmt.Errorf("the server runs %s, and %s is not set", repo.ClaudeCode, EnvClaudeToken)
		}
		hc.Anthropic.TokenFile = secret("claude.token", k)
	}
	if runs(repo.Codex) {
		k := env(EnvCodexAuth)
		if k == "" {
			return nil, fmt.Errorf("the server runs %s, and %s is not set", repo.Codex, EnvCodexAuth)
		}
		hc.Codex.AuthFile = secret("codex-auth.json", k)
	}
	switch {
	case s.GitHub.AppID != 0:
		k := env(EnvAppKey)
		if k == "" {
			return nil, fmt.Errorf("github.app_id is set, and %s is not", EnvAppKey)
		}
		hc.GitHub.AppID = s.GitHub.AppID
		hc.GitHub.PrivateKey = secret("github-app.pem", k)
	case env(EnvToken) != "":
		hc.GitHub.TokenFile = secret("github.token", env(EnvToken))
	default:
		return nil, fmt.Errorf("give github.app_id in the server's file and %s, or %s", EnvAppKey, EnvToken)
	}
	if t := env(EnvOwnerGhAuth); t != "" {
		// Uploads of the bots' images go through the owner's gh, which reads
		// its token from this file by the wrapper deploy writes.
		tok := secret("owner-gh.token", t)
		files = append(files, File{Name: "gh", Mode: 0o700, Data: []byte("#!/bin/sh\nGH_TOKEN=\"$(cat " + strings.Replace(tok, "~", "$HOME", 1) + ")\" exec gh \"$@\"\n")})
		hc.Gh = dir + "/gh"
	}
	if s.API.Port > 0 {
		hc.API = host.API{Listen: fmt.Sprintf("tailnet:%d", s.API.Port), Allow: s.API.Allow}
	}
	data, err := yaml.Marshal(hc)
	if err != nil {
		return nil, err
	}
	files = append(files, File{Name: "host.yaml", Mode: 0o600, Data: append([]byte("# Written by harness deploy. Do not edit: the next deploy replaces it.\n"), data...)})
	return files, nil
}

var script = template.Must(template.New("deploy").Parse(`set -euo pipefail
inst={{.Inst}}
conf="$HOME/.config/harness/$inst"
share="$HOME/.local/share/harness/$inst"
# The binary is where the bots can run it too: the bots' user cannot enter
# this user's home.
bindir="$share"
unit="harness-$inst.service"
units="$HOME/.config/systemd/user"
mkdir -p "$conf" "$share" "$units"
chmod 700 "$conf"
# The service's PATH is the login shell's, so the bots find go and node.
path="$(bash -lc 'printf %s "$PATH"' 2>/dev/null || printf %s "$PATH")"
for c in {{range .Commands}}{{.}} {{end}}; do
	PATH="$path" command -v "$c" >/dev/null || { echo "$c is not installed on $(hostname); install it, then deploy again" >&2; exit 1; }
done
{{if .BotUser}}
# The bots' user: no sudo, no gh login, nothing of this user's.
if ! id -u {{.BotUser}} >/dev/null 2>&1; then
	sudo -n useradd --system --create-home --home-dir /var/lib/{{.BotUser}} --shell /usr/sbin/nologin {{.BotUser}}
fi
id -nG "$USER" | tr ' ' '\n' | grep -qx {{.BotUser}} || sudo -n usermod -aG {{.BotUser}} "$USER"
# A group joined after this user's systemd manager started is not one its
# services get: on the first deploy, the service could not chown the
# worktrees and restarted every few seconds until the manager was restarted.
gid="$(getent group {{.BotUser}} | cut -d: -f3)"
mpid="$(systemctl show "user@$(id -u).service" -p MainPID --value 2>/dev/null || true)"
if [ -n "$mpid" ] && [ "$mpid" != 0 ] && [ -r "/proc/$mpid/status" ] &&
	! sed -n 's/^Groups:[[:space:]]*//p' "/proc/$mpid/status" | tr ' ' '\n' | grep -qx "$gid"; then
	echo "$USER's systemd manager started before $USER joined the group {{.BotUser}}, so the service would not have it. Restart the manager once (sudo systemctl restart user@$(id -u).service, which restarts $USER's user services) or reboot, then deploy again." >&2
	exit 1
fi
sudo -n install -d -o "$USER" -g {{.BotUser}} -m 2770 /var/lib/harness-$inst
mkdir -p /var/lib/harness-$inst/bin
chmod 2750 /var/lib/harness-$inst/bin
bindir=/var/lib/harness-$inst/bin
rule="$USER ALL=({{.BotUser}}) NOPASSWD: ALL"
echo "$rule" | sudo -n tee /etc/sudoers.d/harness-$inst.new >/dev/null
sudo -n visudo -cf /etc/sudoers.d/harness-$inst.new >/dev/null
sudo -n mv /etc/sudoers.d/harness-$inst.new /etc/sudoers.d/harness-$inst
{{end}}
# Keep what is there, to put back if the new one does not come up.
[ -f "$bindir/harness" ] && cp -p "$bindir/harness" "$bindir/harness.previous"
[ -f "$conf/host.yaml" ] && cp -p "$conf/host.yaml" "$conf/host.yaml.previous"
{{range .Files}}
printf %s '{{.B64}}' | base64 -d > "$conf/{{.Name}}.new"
chmod {{.Mode}} "$conf/{{.Name}}.new"
mv "$conf/{{.Name}}.new" "$conf/{{.Name}}"
{{end}}
mv "$share/harness.new" "$bindir/harness"
# A deploy is a person's say: no version the service gave up on stays
# given up on.
rm -f "$bindir/harness.skip"
"$bindir/harness" serve -validate --config "$conf/host.yaml"
# When the service cannot stay up, as after it updated itself to a release
# that fails, the previous binary goes back, and the service keeps off the
# failed release until harness.yaml pins another.
cat > "$conf/rollback" <<'ROLLBACK'
#!/bin/sh
b="$1"
# systemd runs this after every crash, even one it restarts from: only a
# service it gave up on is put back.
[ "$(systemctl --user show "$2" -p ActiveState --value)" = failed ] || exit 0
[ -f "$b.previous" ] || exit 0
"$b" version >> "$b.skip" 2>/dev/null || true
mv "$b.previous" "$b"
systemctl --user reset-failed "$2"
systemctl --user start "$2"
ROLLBACK
chmod 700 "$conf/rollback"
cat > "$units/harness-$inst-rollback.service" <<UNIT
[Unit]
Description=put back the harness before the last one, for {{.Repo}}

[Service]
Type=oneshot
ExecStart=$conf/rollback $bindir/harness $unit
UNIT
cat > "$units/$unit" <<UNIT
[Unit]
Description=harness bots for {{.Repo}}
After=network-online.target
# Five starts in ten minutes, and it is given up on: see the rollback.
StartLimitIntervalSec=10min
StartLimitBurst=5
OnFailure=harness-$inst-rollback.service

[Service]
ExecStart=$bindir/harness serve --config $conf/host.yaml
Environment=PATH=$path
# The state directory is shared with the bots' group.
UMask=0007
# The first SIGTERM lets running turns finish; the stop timeout cuts them off.
KillMode=mixed
TimeoutStopSec=30min
Restart=on-failure
RestartSec=10
# The service exits with 75 once it has updated itself: start the new one.
SuccessExitStatus=75
RestartForceExitStatus=75

[Install]
WantedBy=default.target
UNIT
loginctl enable-linger "$USER" 2>/dev/null || sudo -n loginctl enable-linger "$USER" 2>/dev/null || true
systemctl --user daemon-reload
systemctl --user enable "$unit" >/dev/null
old="$(systemctl --user show "$unit" -p MainPID --value 2>/dev/null || true)"
systemctl --user restart --no-block "$unit"
# The old service lets its running turns finish before it stops, for as
# long as TimeoutStopSec: that wait is not the new one failing to come up.
ok= said= i=0
while [ "$i" -lt "${DEPLOY_WAIT:-20}" ]; do
	sleep 1
	state="$(systemctl --user show "$unit" -p ActiveState --value 2>/dev/null || true)"
	if [ "$state" = deactivating ]; then
		[ -n "$said" ] || echo "waiting for the running turns to finish; the new service starts after them, in at most 30 minutes" >&2
		said=1
		continue
	fi
	i=$((i + 1))
	pid="$(systemctl --user show "$unit" -p MainPID --value 2>/dev/null || true)"
	if systemctl --user is-active --quiet "$unit" && { [ -z "$old" ] || [ "$old" = 0 ] || [ "$pid" != "$old" ]; } &&
		[ "$("$bindir/harness" version)" = {{.Version}} ]; then
		ok=1
		break
	fi
done
if [ -z "$ok" ]; then
	echo "the service did not come up; putting the previous one back" >&2
	journalctl --user -u "$unit" -n 30 --no-pager >&2 || true
	[ -f "$bindir/harness.previous" ] && mv "$bindir/harness.previous" "$bindir/harness"
	[ -f "$conf/host.yaml.previous" ] && mv "$conf/host.yaml.previous" "$conf/host.yaml"
	systemctl --user restart "$unit" || true
	exit 1
fi
`))

// Script is what runs on a server to install the instance, once the binary
// is at harness.new beside where it goes.
func Script(inst, repoName string, s *Server, files []File, version string) (string, error) {
	type file struct {
		Name, B64, Mode string
	}
	var fs []file
	for _, f := range files {
		fs = append(fs, file{Name: f.Name, B64: base64.StdEncoding.EncodeToString(f.Data), Mode: fmt.Sprintf("%o", f.Mode)})
	}
	var cmds []string
	for _, a := range s.Agents {
		cmds = append(cmds, host.DefaultCommands[a])
	}
	cmds = append(cmds, "git")
	var b strings.Builder
	err := script.Execute(&b, map[string]any{
		"Inst": inst, "BotUser": s.BotUser, "Files": fs, "Commands": cmds, "Version": shellQuote(version), "Repo": repoName,
	})
	return b.String(), err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
