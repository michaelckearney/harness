package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Call is one verb, as a Go framework receives it from the harness.
type Call struct {
	Verb string
	Args []string
	// Root, Framework, and Project are folders; Project is "" for the
	// framework itself.
	Root, Framework, Project string
	// Name is the project's name.
	Name string
	// Cwd is where the harness was run.
	Cwd string
	// Version is set for build.
	Version string
	// Turn is true during a bot's turn.
	Turn bool
}

// Path makes an argument that is a path relative to where the harness ran
// into one relative to the project, which is where a framework works.
func (c Call) Path(arg string) string {
	base := c.Project
	if base == "" {
		base = c.Framework
	}
	abs := arg
	if !filepath.IsAbs(arg) {
		abs = filepath.Join(c.Cwd, arg)
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return arg
	}
	return rel
}

// Handler implements one verb. A returned error fails the verb; its
// message is printed for whoever ran the harness.
type Handler func(Call) (Result, error)

// Main runs a framework written in Go: it reads the call from the
// arguments and the environment, runs the verb's handler, writes its
// result, and exits.
func Main(handlers map[string]Handler) {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <verb> [args...], run by the harness; the verbs are %s\n", filepath.Base(os.Args[0]), verbList(handlers))
		os.Exit(2)
	}
	verb := os.Args[1]
	h, ok := handlers[verb]
	if !ok {
		fmt.Fprintf(os.Stderr, "this framework does not implement %q; it implements %s\n", verb, verbList(handlers))
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	c := Call{
		Verb:      verb,
		Args:      os.Args[2:],
		Root:      os.Getenv(EnvRoot),
		Framework: os.Getenv(EnvFramework),
		Project:   os.Getenv(EnvProject),
		Name:      os.Getenv(EnvProjectName),
		Cwd:       os.Getenv(EnvCwd),
		Version:   os.Getenv(EnvVersion),
		Turn:      os.Getenv(EnvTurn) == "1",
	}
	if c.Framework == "" {
		c.Framework = cwd
	}
	if c.Cwd == "" {
		c.Cwd = cwd
	}
	r, err := h(c)
	if werr := Write(r); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func verbList(handlers map[string]Handler) string {
	var vs []string
	for v := range handlers {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return strings.Join(vs, ", ")
}
