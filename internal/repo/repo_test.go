package repo

import (
	"strings"
	"testing"
)

const good = `schema: 1
repo: o/r
triggers: [owner]
agents: { default: opencode, allowed: [opencode, claude-code] }
projects:
  harbor: projects/harbor
  slipway: projects/slipway
  site: projects/site
`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Framework != "framework" || c.Projects["harbor"].Path != "projects/harbor" {
		t.Errorf("framework and projects: %s %+v", c.Framework, c.Projects)
	}
	for _, bad := range []struct{ change, want string }{
		{"projects:|servers: {}\nprojects:", "servers: is gone"},
		{"projects:|model: { id: m }\nprojects:", "model: is gone"},
		{"site: projects/site|site: projects/harbor/site", "overlap"},
		{"site: projects/site|site: framework/site", "overlap"},
		{"site: projects/site|site: { path: projects/site }", "a project is its folder"},
		{"projects:|framework: framework\nprojects:", "framework: is gone"},
		{"triggers: [owner]|triggers: []", "triggers"},
		{"schema: 1|schema: 1\nextra: 1", "extra"},
	} {
		from, to, _ := strings.Cut(bad.change, "|")
		if _, err := Parse([]byte(strings.Replace(good, from, to, 1))); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: %v, want %q", bad.change, err, bad.want)
		}
	}
}

func TestScopes(t *testing.T) {
	c, _ := Parse([]byte(good))
	if s := c.ScopeOf("projects/harbor/backend/x.go"); s.Kind != ProjectKind || s.Name != "harbor" {
		t.Errorf("scope of a project's file: %+v", s)
	}
	for _, rel := range []string{"framework", "README.md", "."} {
		if s := c.ScopeOf(rel); s.Kind != FrameworkKind {
			t.Errorf("scope of %s: %+v", rel, s)
		}
	}
	fw, err := c.ScopeNamed("framework")
	if err != nil {
		t.Fatal(err)
	}
	if fw.Label() != "framework" || fw.String() != "the framework" {
		t.Errorf("framework scope: %q %q", fw.Label(), fw)
	}
	// The framework's bot changes every project too, and nothing else.
	for file, want := range map[string]bool{
		"framework/x.go": true, "projects/harbor/a": true, "projects/site/c": true,
		"harness.yaml": false, ".github/workflows/pr.yml": false, "frameworkx/y": false,
	} {
		if fw.Writes(file) != want {
			t.Errorf("framework writes %s: %v", file, !want)
		}
	}
	if got := names(c.Targets(fw, true)); got != "the framework, project harbor, project site, project slipway" {
		t.Errorf("targets of the framework: %s", got)
	}
	harbor, _ := c.ScopeNamed("project:harbor")
	if harbor.Label() != "project:harbor" || !harbor.Writes("projects/harbor/x") || harbor.Writes("projects/site/x") {
		t.Errorf("project scope: %+v", harbor)
	}
	if !c.Covers(fw, harbor) || c.Covers(harbor, fw) || !c.Covers(harbor, harbor) {
		t.Error("Covers is wrong")
	}
	for _, bad := range []string{"project:nope", "framework:desktop", "harbor", ""} {
		if _, err := c.ScopeNamed(bad); err == nil {
			t.Errorf("%q was a scope", bad)
		}
	}
}

func TestAffected(t *testing.T) {
	c, _ := Parse([]byte(good))
	fw := c.FrameworkScope()
	harbor, _ := c.ScopeNamed("project:harbor")
	for _, tc := range []struct {
		scope   Scope
		changed []string
		want    string
	}{
		{fw, []string{"projects/harbor/a.go", "projects/site/b.go"}, "project harbor, project site"},
		{fw, []string{"projects/harbor/a.go", "framework/x.go"}, "the framework, project harbor, project site, project slipway"},
		{fw, nil, "the framework, project harbor, project site, project slipway"},
		{harbor, []string{"projects/harbor/a.go"}, "project harbor"},
		{harbor, nil, "project harbor"},
	} {
		if got := names(c.Affected(tc.scope, tc.changed)); got != tc.want {
			t.Errorf("%s with %v: %s, want %s", tc.scope, tc.changed, got, tc.want)
		}
	}
}

func names(ts []Target) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.String())
	}
	return strings.Join(out, ", ")
}
