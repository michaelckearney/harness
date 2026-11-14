package service

import (
	"testing"

	"github.com/michaelckearney/harness/internal/repo"
)

func TestUnrelatedChangesAreMergedForTheBot(t *testing.T) {
	cfg, err := repo.Parse([]byte("schema: 1\nrepo: o/r\ntriggers: [owner]\nagents: { default: claude-code }\n" +
		"projects: { harbor: projects/harbor, slipway: projects/slipway }\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		label, files string
		want         bool
	}{
		{"project:harbor", "projects/slipway/a.go\nprojects/slipway/b.vue", true},
		{"project:harbor", "projects/slipway/a.go\nprojects/harbor/b.go", false},
		{"project:harbor", "projects/slipway/a.go\nframework/x.go", false},
		{"project:harbor", "harness.yaml", false},
		{"project:harbor", "", false},
		{"framework", "projects/slipway/a.go", false},
		{"", "projects/slipway/a.go", false},
	} {
		if got := unrelated(cfg, c.label, c.files); got != c.want {
			t.Errorf("%s with %q: %v, want %v", c.label, c.files, got, c.want)
		}
	}
}
