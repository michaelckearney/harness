// Package host reads a server's configuration for one monorepo: what
// `harness serve` needs that the monorepo's harness.yaml does not say, and
// the secrets. `harness deploy` writes it, on the server, from the server's
// own file and secrets; nothing in a bot's reach can change it.
//
// One server runs one service per monorepo, each with its own
// configuration, state, and bots' user, so two monorepos on one server,
// such as two clients', share nothing.
package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/michaelckearney/harness/internal/repo"
)

// Config is the host configuration file.
//
//	schema: 1
//	name: harness-poc
//	repo: owner/name
//	agents: [opencode]
//	model: { base_url: http://127.0.0.1:8000/v1, id: qwen3.6, vision: true }
//	github: { app_id: 123456, private_key: ~/.config/harness/harness-poc/github-app.pem }
type Config struct {
	Schema int `yaml:"schema"`
	// Name names this monorepo's service and files on the server.
	Name string `yaml:"name"`
	// Repo is owner/name on GitHub.
	Repo string `yaml:"repo"`
	// GitURL is where the repository is cloned from and pushed to. It
	// defaults to the repository on github.com.
	GitURL string `yaml:"git_url"`
	// StateDir holds the database, the socket, the clone, and the worktrees.
	StateDir string `yaml:"state_dir"`
	// Poll is how often GitHub is asked what changed.
	Poll time.Duration `yaml:"poll"`
	// Sessions is how many turns run at once.
	Sessions int `yaml:"sessions"`
	// Agents are the agents this server runs: the threads whose agent is
	// another are another server's.
	Agents []string `yaml:"agents"`
	// Commands are the agents' commands, by agent. Each defaults to the
	// agent's usual command on PATH.
	Commands map[string]string `yaml:"commands"`
	// User, if set, is who the agents run as, and so every command a bot
	// runs: started with sudo -n, which must allow it without a password.
	User string `yaml:"user"`
	// Group is shared by the service's user and User: the socket and the
	// state directory are the group's. Required with User.
	Group string `yaml:"group"`
	// Env is added to the bots' environment.
	Env map[string]string `yaml:"env"`
	// Model is the OpenAI-compatible endpoint opencode uses.
	Model Model `yaml:"model"`
	// Anthropic is for Claude Code, Codex for Codex.
	Anthropic Anthropic `yaml:"anthropic"`
	Codex     Codex     `yaml:"codex"`
	// Gh is the gh command, logged in as the owner. The service asks it for
	// its token to upload the files the bots put in what they post:
	// GitHub's upload endpoint refuses App tokens.
	Gh string `yaml:"gh"`
	// Author is who the bots' commits are by.
	Author Author `yaml:"author"`
	// API is what the Slipway app reads. Off unless configured.
	API    API    `yaml:"api"`
	GitHub GitHub `yaml:"github"`
}

// Model is an OpenAI-compatible endpoint.
type Model struct {
	BaseURL string `yaml:"base_url"`
	ID      string `yaml:"id"`
	Vision  bool   `yaml:"vision"`
	Context int    `yaml:"context"`
	// KeyFile, if the endpoint needs a key, holds it.
	KeyFile string `yaml:"key_file"`
}

// Anthropic is Claude Code's API: where it is, and the file with the
// Claude subscription's token, from `claude setup-token`. The proxy holds
// the token, so Claude Code never sees it.
type Anthropic struct {
	BaseURL   string `yaml:"base_url"`
	TokenFile string `yaml:"token_file"`
}

// Codex is how Codex signs in: with ChatGPT. AuthFile is the auth.json
// that `codex login` writes. The service copies it into Codex's home when
// it changes, and from then on Codex refreshes its own copy.
type Codex struct {
	AuthFile string `yaml:"auth_file"`
}

// API serves the Slipway app: read-only, apart from stopping and retrying a
// turn, and only on the tailnet.
type API struct {
	// Listen is host:port. A host of "tailnet" is this machine's Tailscale
	// IPv4 address, so the API is never on another network.
	Listen string `yaml:"listen"`
	// Allow is the Tailscale logins that may use it.
	Allow []string `yaml:"allow"`
	// Tailscale is the tailscale command.
	Tailscale string `yaml:"tailscale"`
}

type Author struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// GitHub says how the service signs in: as a GitHub App, or with a token
// read from a file.
type GitHub struct {
	AppID      int64  `yaml:"app_id"`
	PrivateKey string `yaml:"private_key"`
	TokenFile  string `yaml:"token_file"`
	APIURL     string `yaml:"api_url"`
}

var (
	validRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// DefaultCommands are the agents' commands when the configuration names
// none.
var DefaultCommands = map[string]string{
	repo.OpenCode:   "opencode",
	repo.ClaudeCode: "claude",
	repo.Codex:      "codex",
}

// Load reads and checks a host configuration, and fills in the defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) check() error {
	if c.Schema != 1 {
		return fmt.Errorf("schema must be 1")
	}
	if !validRepo.MatchString(c.Repo) {
		return fmt.Errorf("repo %q must be owner/name", c.Repo)
	}
	if c.Name == "" {
		c.Name = strings.ToLower(strings.SplitN(c.Repo, "/", 2)[1])
	}
	if !validName.MatchString(c.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits, '-' or '_'", c.Name)
	}
	if c.GitURL == "" {
		c.GitURL = "https://github.com/" + c.Repo + ".git"
	}
	home, _ := os.UserHomeDir()
	if c.StateDir == "" {
		c.StateDir = filepath.Join(home, ".local", "state", "harness", c.Name)
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("agents: list the agents this server runs")
	}
	if c.Commands == nil {
		c.Commands = map[string]string{}
	}
	for _, a := range c.Agents {
		if !slices.Contains(repo.AllAgents, a) {
			return fmt.Errorf("agents: %q is not an agent; the agents are %s", a, strings.Join(repo.AllAgents, ", "))
		}
		if c.Commands[a] == "" {
			c.Commands[a] = DefaultCommands[a]
		}
		c.Commands[a] = expand(c.Commands[a])
	}
	if c.Runs(repo.OpenCode) && (c.Model.ID == "" || c.Model.BaseURL == "") {
		return fmt.Errorf("model: opencode needs id and base_url")
	}
	if c.Anthropic.BaseURL == "" {
		c.Anthropic.BaseURL = "https://api.anthropic.com"
	}
	if c.Runs(repo.ClaudeCode) && c.Anthropic.TokenFile == "" {
		return fmt.Errorf("anthropic.token_file: Claude Code needs a Claude subscription token")
	}
	if c.Runs(repo.Codex) && c.Codex.AuthFile == "" {
		return fmt.Errorf("codex.auth_file: Codex needs a ChatGPT sign-in")
	}
	for _, f := range []*string{&c.Model.KeyFile, &c.Anthropic.TokenFile, &c.Codex.AuthFile} {
		*f = expand(*f)
		if *f != "" {
			if _, err := os.Stat(*f); err != nil {
				return err
			}
		}
	}
	if (c.User == "") != (c.Group == "") {
		return fmt.Errorf("give user and group together, or neither")
	}
	if c.Gh == "" {
		c.Gh = "gh"
	}
	c.StateDir, c.Gh = expand(c.StateDir), expand(c.Gh)
	if c.Poll == 0 {
		c.Poll = time.Minute
	}
	if c.Poll < 5*time.Second {
		return fmt.Errorf("poll must be at least 5s")
	}
	if c.Sessions == 0 {
		c.Sessions = 1
	}
	if c.Sessions < 0 {
		return fmt.Errorf("sessions must be 1 or more")
	}
	if c.API.Listen != "" {
		if len(c.API.Allow) == 0 {
			return fmt.Errorf("api.allow: list the Tailscale logins that may use the API")
		}
		if c.API.Tailscale == "" {
			c.API.Tailscale = "tailscale"
		}
	}
	if c.Author.Name == "" {
		c.Author.Name = "harness"
	}
	if c.Author.Email == "" {
		host, _ := os.Hostname()
		c.Author.Email = "harness@" + host
	}

	g := &c.GitHub
	if g.APIURL == "" {
		g.APIURL = "https://api.github.com"
	}
	g.APIURL = strings.TrimSuffix(g.APIURL, "/")
	g.PrivateKey, g.TokenFile = expand(g.PrivateKey), expand(g.TokenFile)
	switch {
	case g.TokenFile != "" && (g.AppID != 0 || g.PrivateKey != ""):
		return fmt.Errorf("github: give app_id and private_key, or token_file, not both")
	case g.TokenFile != "":
		if _, err := os.Stat(g.TokenFile); err != nil {
			return fmt.Errorf("github.token_file: %w", err)
		}
	case g.AppID == 0 || g.PrivateKey == "":
		return fmt.Errorf("github: give app_id and private_key, or token_file")
	default:
		if _, err := os.Stat(g.PrivateKey); err != nil {
			return fmt.Errorf("github.private_key: %w", err)
		}
	}
	return nil
}

// Runs reports whether this server runs an agent.
func (c *Config) Runs(agent string) bool { return slices.Contains(c.Agents, agent) }

// Socket is where the service listens for the bots' commands.
func (c *Config) Socket() string { return filepath.Join(c.StateDir, "harness.sock") }

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// SocketEnv names the service's socket in a bot's environment. It is set
// only during a bot's turn, so it is also how the harness knows a bot is
// running it.
const SocketEnv = "HARNESS_BOT_SOCKET"

// TurnSocket is the socket in a bot's environment, or "" outside a turn.
func TurnSocket() string { return os.Getenv(SocketEnv) }
