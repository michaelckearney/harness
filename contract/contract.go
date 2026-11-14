// Package contract is what the harness and a framework say to each other.
//
// A framework is a folder in a monorepo with a framework.yaml:
//
//	command: [go, run, ./cmd/desktop]
//	verbs:
//	  test: lint, unit tests, then e2e tests
//	  build: build the release artifacts
//	  run: start the app for development
//	  record: { help: drive the app and record it, as evidence, turn: true }
//
// The harness runs the command in the framework's folder with the verb and
// its arguments after it, and the environment below. The command's output
// goes to whoever ran the harness, and its exit code says whether the verb
// worked. A verb that has more to say, such as whether test ran everything,
// writes a Result as JSON to the file HARNESS_RESULT names.
//
// Two verbs are the harness's, the same in every monorepo: test, which
// every framework implements and a bot's done relies on, and build, which
// releases rely on. Every other verb is the framework's own: the harness
// passes it on and lists it in harness help, and knows nothing more of it.
// A framework is free in everything else, such as its layout, its tools,
// and the arguments its verbs take.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"
)

// The core verbs, which mean the same in every framework.
const (
	// Test checks a project, or the framework itself: lint and tests. With
	// no arguments it runs everything, and says so with Result.Full. Every
	// framework implements it, and a bot may run it in its turn.
	Test = "test"
	// Build makes a project's release artifacts, and lists them in
	// Result.Artifacts. HARNESS_VERSION is the version to build. Releases
	// run it; a bot does not.
	Build = "build"
)

// Reserved are the harness's own commands, which no framework verb may
// take.
var Reserved = []string{"check", "release", "init", "deploy", "serve", "queue", "version", "help",
	"status", "diff", "commit", "sync", "reply", "done", "scope"}

var validVerb = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// The environment the harness gives a framework's command.
const (
	// EnvRoot is the monorepo's root.
	EnvRoot = "HARNESS_ROOT"
	// EnvFramework is the framework's folder.
	EnvFramework = "HARNESS_FRAMEWORK"
	// EnvProject is the project's folder, or empty when the verb is for the
	// framework itself, such as test on the framework's own code.
	EnvProject = "HARNESS_PROJECT"
	// EnvProjectName is the project's name in harness.yaml.
	EnvProjectName = "HARNESS_PROJECT_NAME"
	// EnvCwd is where the harness was run, for arguments that are paths.
	EnvCwd = "HARNESS_CWD"
	// EnvResult is the file to write the Result to.
	EnvResult = "HARNESS_RESULT"
	// EnvVersion is the version to build, such as 0.4.0 or
	// 0.4.1-dev.abc1234. Set for build.
	EnvVersion = "HARNESS_VERSION"
	// EnvTurn is "1" during a bot's turn.
	EnvTurn = "HARNESS_TURN"
)

// Result is what a verb says besides its exit code.
type Result struct {
	// Full is true when test ran everything: lint and every test, with no
	// filter. Only a full run counts as tested for a bot's done.
	Full bool `json:"full,omitempty"`
	// Artifacts are the files build made for a release, as paths relative
	// to the project's folder.
	Artifacts []string `json:"artifacts,omitempty"`
	// Summary is one line on what happened, for the harness to show.
	Summary string `json:"summary,omitempty"`
}

// Write writes r to the file the harness asked for. Without one, as when
// the framework runs on its own, it does nothing.
func Write(r Result) error {
	path := os.Getenv(EnvResult)
	if path == "" {
		return nil
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Read reads the Result a verb wrote, or a zero Result if it wrote none.
func Read(path string) (Result, error) {
	var r Result
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(bytes.TrimSpace(data)) == 0) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("the framework's result is not valid JSON: %w", err)
	}
	return r, nil
}

// ManifestFile is the file that makes a folder a framework.
const ManifestFile = "framework.yaml"

// Manifest is a framework.yaml.
type Manifest struct {
	// Command is how to run the framework, from its folder.
	Command []string `yaml:"command"`
	// Verbs are the verbs it implements, by name.
	Verbs map[string]Verb `yaml:"verbs"`
}

// Verb is one of a framework's verbs. In framework.yaml it is its help
// alone, or { help: ..., turn: true }.
type Verb struct {
	// Help is one line on what it does, for harness help.
	Help string `yaml:"help"`
	// Turn lets a bot run it in its turn. Test always may, and build
	// never; another verb may only if it returns, and does nothing that is
	// a person's to do.
	Turn bool `yaml:"turn"`
}

func (v *Verb) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&v.Help)
	}
	type plain Verb
	return n.Decode((*plain)(v))
}

// Supports reports whether the framework implements a verb.
func (m *Manifest) Supports(verb string) bool {
	_, ok := m.Verbs[verb]
	return ok
}

// InTurn reports whether a bot may run a verb in its turn.
func (m *Manifest) InTurn(verb string) bool {
	switch verb {
	case Test:
		return m.Supports(Test)
	case Build:
		return false
	}
	return m.Verbs[verb].Turn
}

// Names are the verbs, test and build first, then the framework's own in
// order.
func (m *Manifest) Names() []string {
	var out, own []string
	for _, v := range []string{Test, Build} {
		if m.Supports(v) {
			out = append(out, v)
		}
	}
	for v := range m.Verbs {
		if v != Test && v != Build {
			own = append(own, v)
		}
	}
	slices.Sort(own)
	return append(out, own...)
}

// LoadManifest reads the framework.yaml in dir.
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(m.Command) == 0 {
		return nil, fmt.Errorf("%s: command is empty; give how to run the framework, such as [go, run, ./cmd/<name>]", path)
	}
	for v := range m.Verbs {
		if !validVerb.MatchString(v) {
			return nil, fmt.Errorf("%s: verb %q must be lowercase letters, digits, or '-'", path, v)
		}
		if slices.Contains(Reserved, v) {
			return nil, fmt.Errorf("%s: %q is the harness's own command; give the verb another name", path, v)
		}
	}
	if !m.Supports(Test) {
		return nil, fmt.Errorf("%s: every framework implements test", path)
	}
	return &m, nil
}
