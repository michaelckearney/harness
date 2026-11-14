// Package repo reads a monorepo's harness.yaml: the harness it uses, its
// framework and projects, who may give its bots work, and which agents they
// may be. Nothing in it is about a machine: the servers the bots run on, the
// model they use, and their keys are each server's own (see harness deploy).
//
//	schema: 1
//	harness: v0.7.0
//	repo: owner/name
//	triggers: [owner]
//	agents:
//	  default: opencode
//	  allowed: [opencode, claude-code, codex]
//	projects:
//	  harbor: projects/harbor
//
// The framework is always in framework/, with its framework.yaml: a monorepo
// has one.
//
// The service reads it from the default branch only, so a change takes
// effect when a human merges it.
package repo

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is the monorepo's configuration, at its root.
const File = "harness.yaml"

// FrameworkDir is the folder with the monorepo's framework.yaml, at its
// root: the code and tooling every project shares.
const FrameworkDir = "framework"

// The agents the harness can run.
const (
	OpenCode   = "opencode"
	ClaudeCode = "claude-code"
	Codex      = "openai-codex"
)

// AllAgents is every agent the harness can run.
var AllAgents = []string{OpenCode, ClaudeCode, Codex}

type Config struct {
	Schema int `yaml:"schema"`
	// Harness is the version of the harness the monorepo uses, such as
	// v0.1.0: what its pipelines install, and what its servers update
	// themselves to when it changes on the default branch.
	Harness string `yaml:"harness"`
	// Repo is owner/name on GitHub.
	Repo string `yaml:"repo"`
	// Triggers are the GitHub logins who can give the bots work. Only
	// their issues, comments, reviews, and labels count.
	Triggers []string `yaml:"triggers"`
	Agents   Agents   `yaml:"agents"`
	// Prepare is a shell command run in a bot's worktree before each turn,
	// with the service's credentials, such as go mod download for a
	// private dependency the bot cannot fetch.
	Prepare string `yaml:"prepare"`
	// Framework is FrameworkDir; it is not in harness.yaml.
	Framework string `yaml:"-"`
	// OldFramework is the framework: key harness.yaml used to have, there
	// only to say it is gone.
	OldFramework string `yaml:"framework"`
	// The machine's keys harness.yaml used to have, there only to say
	// where they went.
	OldModel   any `yaml:"model"`
	OldGitHub  any `yaml:"github"`
	OldAPI     any `yaml:"api"`
	OldServers any `yaml:"servers"`
	// Projects are the folders the framework builds, by name.
	Projects map[string]*Project `yaml:"projects"`
}

// Agents says which agents the bots may be.
type Agents struct {
	// Default works an issue with no agent label.
	Default string `yaml:"default"`
	// Allowed are the agents an issue's label may choose. An agent that
	// sends the code to a third party belongs here only if whoever owns the
	// code agreed to it.
	Allowed []string `yaml:"allowed"`
}

// Project is a folder the framework builds. In harness.yaml it is the
// folder alone.
type Project struct {
	Path string
}

func (p *Project) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a project is its folder, such as projects/harbor", n.Line)
	}
	return n.Decode(&p.Path)
}

var (
	validRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// Error is a problem in harness.yaml.
type Error struct{ Msg string }

func (e *Error) Error() string { return File + ": " + e.Msg }

func fail(format string, args ...any) error { return &Error{fmt.Sprintf(format, args...)} }

// Parse reads and checks a harness.yaml.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fail("%v", err)
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) check() error {
	if c.Schema != 1 {
		return fail("schema must be 1")
	}
	if !validRepo.MatchString(c.Repo) {
		return fail("repo %q must be owner/name", c.Repo)
	}
	if len(c.Triggers) == 0 {
		return fail("triggers: list who can give the bots work")
	}
	a := &c.Agents
	if a.Default == "" {
		a.Default = OpenCode
	}
	if len(a.Allowed) == 0 {
		a.Allowed = []string{a.Default}
	}
	for _, name := range a.Allowed {
		if !slices.Contains(AllAgents, name) {
			return fail("agents.allowed: %q is not an agent; the agents are %s", name, strings.Join(AllAgents, ", "))
		}
	}
	if !slices.Contains(a.Allowed, a.Default) {
		return fail("agents.default %q must be one of agents.allowed", a.Default)
	}

	paths := map[string]string{}
	claim := func(what, p string) error {
		if p == "" || path.IsAbs(p) || strings.HasPrefix(path.Clean(p), "..") || path.Clean(p) == "." {
			return fail("%s: path %q must be a folder inside the repository", what, p)
		}
		clean := path.Clean(p)
		for other, by := range paths {
			if clean == other || strings.HasPrefix(clean, other+"/") || strings.HasPrefix(other, clean+"/") {
				return fail("%s and %s overlap: %s and %s", what, by, clean, other)
			}
		}
		paths[clean] = what
		return nil
	}
	for key, n := range map[string]any{"model": c.OldModel, "github": c.OldGitHub, "api": c.OldAPI, "servers": c.OldServers} {
		if n != nil {
			return fail("%s: is gone: it is each server's now, in the server file harness deploy reads; move it there and remove it here", key)
		}
	}
	if c.OldFramework != "" {
		return fail("framework: is gone: the framework is always in %s/; move it there and remove the line", FrameworkDir)
	}
	c.Framework = FrameworkDir
	if err := claim("framework", c.Framework); err != nil {
		return err
	}
	for _, name := range sortedKeys(c.Projects) {
		p := c.Projects[name]
		if !validName.MatchString(name) {
			return fail("projects: %q must be lowercase letters, digits, '-' or '_'", name)
		}
		if p == nil {
			return fail("projects.%s: give its folder", name)
		}
		if err := claim("projects."+name, p.Path); err != nil {
			return err
		}
		p.Path = path.Clean(p.Path)
	}
	return nil
}

// Trusted reports whether a GitHub login can give the bots work.
func (c *Config) Trusted(login string) bool {
	return slices.ContainsFunc(c.Triggers, func(u string) bool { return strings.EqualFold(u, login) })
}

// Load reads the harness.yaml at a monorepo's root.
func Load(root string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(root, File))
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// ErrNoRoot is returned outside a monorepo.
var ErrNoRoot = errors.New("not in a monorepo: no " + File + " here or in a folder above")

// FindRoot is the monorepo dir is in: the nearest folder at or above it
// with a harness.yaml.
func FindRoot(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, File)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNoRoot
		}
		dir = parent
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ProjectNames are the projects, sorted.
func (c *Config) ProjectNames() []string { return sortedKeys(c.Projects) }
