package repo

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// The kinds of scope.
const (
	// FrameworkKind is the framework: its own code, and every project,
	// since a change to it can break them.
	FrameworkKind = "framework"
	// ProjectKind is one project.
	ProjectKind = "project"
)

// Scope is what a bot works on, or what a developer's command is for: one
// project, or the framework and every project.
type Scope struct {
	Kind string `json:"kind"`
	// Name is the project's; "" for the framework.
	Name string `json:"name,omitempty"`
	// Path is the scope's folder, relative to the root.
	Path string `json:"path,omitempty"`
	// Paths are the folders a bot working on the scope may change. The
	// framework's include every project's: a breaking change to the
	// framework and the changes to the projects land together.
	Paths []string `json:"paths,omitempty"`
}

func (s Scope) String() string {
	switch s.Kind {
	case FrameworkKind:
		return "the framework"
	case ProjectKind:
		return "project " + s.Name
	}
	return "nothing: the issue has no scope label"
}

// Label is the issue label that picks the scope: framework, or
// project:<name>.
func (s Scope) Label() string {
	if s.Kind == FrameworkKind {
		return FrameworkKind
	}
	return ProjectKind + ":" + s.Name
}

// Writes reports whether a bot working on s may change a file, by its path
// relative to the root.
func (s Scope) Writes(file string) bool {
	file = path.Clean(filepath.ToSlash(file))
	for _, dir := range s.Paths {
		if file == dir || strings.HasPrefix(file, dir+"/") {
			return true
		}
	}
	return false
}

func (c *Config) project(name string) Scope {
	p := c.Projects[name]
	return Scope{Kind: ProjectKind, Name: name, Path: p.Path, Paths: []string{p.Path}}
}

// FrameworkScope is the framework and every project.
func (c *Config) FrameworkScope() Scope {
	s := Scope{Kind: FrameworkKind, Path: c.Framework, Paths: []string{c.Framework}}
	for _, name := range c.ProjectNames() {
		s.Paths = append(s.Paths, c.Projects[name].Path)
	}
	return s
}

// projectOf is the project whose folder holds a path, or "".
func (c *Config) projectOf(rel string) string {
	rel = path.Clean(filepath.ToSlash(rel))
	for _, name := range c.ProjectNames() {
		if dir := c.Projects[name].Path; rel == dir || strings.HasPrefix(rel, dir+"/") {
			return name
		}
	}
	return ""
}

// ScopeOf is the scope a path inside the monorepo belongs to: the project
// whose folder holds it, else the framework.
func (c *Config) ScopeOf(rel string) Scope {
	if name := c.projectOf(rel); name != "" {
		return c.project(name)
	}
	return c.FrameworkScope()
}

// ScopeNamed reads a label: framework, or project:<name>.
func (c *Config) ScopeNamed(label string) (Scope, error) {
	if label == FrameworkKind {
		return c.FrameworkScope(), nil
	}
	kind, name, ok := strings.Cut(label, ":")
	switch {
	case ok && kind == ProjectKind:
		if c.Projects[name] != nil {
			return c.project(name), nil
		}
		return Scope{}, fmt.Errorf("there is no project %q in %s; the projects are %s", name, File, strings.Join(c.ProjectNames(), ", "))
	case ok && kind == FrameworkKind:
		return Scope{}, fmt.Errorf("%q names a framework, and a monorepo has one: the label is framework alone", label)
	}
	return Scope{}, fmt.Errorf("%q is not a scope; a scope is framework, or project:<name>", label)
}

// Target is one thing a verb runs on: a project, or the framework's own
// code, when Project is "".
type Target struct {
	FrameworkPath string // relative to the root
	Project       string // its name, or "" for the framework itself
	ProjectPath   string // relative to the root
}

func (t Target) String() string {
	if t.Project == "" {
		return "the framework"
	}
	return "project " + t.Project
}

// Covers reports whether running a verb on every target of s also covers
// scope o: what a full test of s proves.
func (c *Config) Covers(s, o Scope) bool {
	return s.Kind == FrameworkKind || (s.Kind == o.Kind && s.Name == o.Name)
}

// Targets are what a verb on scope s runs on. For test, the framework's own
// code is a target; for the verbs that need a project, such as run, it is
// not.
func (c *Config) Targets(s Scope, withFramework bool) []Target {
	if s.Kind == ProjectKind {
		return []Target{c.target(s.Name)}
	}
	var out []Target
	if withFramework {
		out = append(out, Target{FrameworkPath: c.Framework})
	}
	for _, name := range c.ProjectNames() {
		out = append(out, c.target(name))
	}
	return out
}

// Affected are the targets a test of scope s needs, given the files the
// work changed, relative to the root: for a project's file, that project;
// for any other file, the framework and every project. With nothing
// changed, it is every target of s.
func (c *Config) Affected(s Scope, changed []string) []Target {
	if s.Kind == ProjectKind || len(changed) == 0 {
		return c.Targets(s, true)
	}
	hit := map[string]bool{}
	for _, f := range changed {
		name := c.projectOf(f)
		if name == "" {
			return c.Targets(s, true)
		}
		hit[name] = true
	}
	var out []Target
	for _, name := range c.ProjectNames() {
		if hit[name] {
			out = append(out, c.target(name))
		}
	}
	return out
}

func (c *Config) target(name string) Target {
	return Target{FrameworkPath: c.Framework, Project: name, ProjectPath: c.Projects[name].Path}
}

// ProjectTarget is the target for one project, by name.
func (c *Config) ProjectTarget(name string) (Target, error) {
	if c.Projects[name] == nil {
		return Target{}, fmt.Errorf("there is no project %q; the projects are %s", name, strings.Join(c.ProjectNames(), ", "))
	}
	return c.target(name), nil
}
