package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/michaelckearney/harness/contract"
	"github.com/michaelckearney/harness/internal/image"
	"github.com/michaelckearney/harness/internal/repo"
)

// Check runs `harness check`: harness.yaml is valid, the framework has a
// valid framework.yaml, and each project's folder is there.
func Check(out io.Writer) error {
	cwd, _ := os.Getwd()
	root, err := repo.FindRoot(cwd)
	if err != nil {
		return &Error{err.Error()}
	}
	cfg, err := repo.Load(root)
	if err != nil {
		return &Error{err.Error()}
	}
	var problems []string
	if m, err := contract.LoadManifest(filepath.Join(root, cfg.Framework)); err != nil {
		problems = append(problems, fmt.Sprintf("framework: %v", err))
	} else {
		fmt.Fprintf(out, "framework           %s  verbs: %s\n", cfg.Framework, strings.Join(m.Names(), ", "))
	}
	if _, err := os.Stat(filepath.Join(root, image.Dir, "Dockerfile")); err != nil {
		problems = append(problems, fmt.Sprintf("%s/Dockerfile: the image the pipelines run the framework in is not there", image.Dir))
	}
	for _, name := range cfg.ProjectNames() {
		p := cfg.Projects[name]
		if info, err := os.Stat(filepath.Join(root, p.Path)); err != nil || !info.IsDir() {
			problems = append(problems, fmt.Sprintf("project %s: %s is not a folder", name, p.Path))
			continue
		}
		fmt.Fprintf(out, "project   %-12s %s\n", name, p.Path)
	}
	if len(problems) > 0 {
		return fail("%s", strings.Join(problems, "\n"))
	}
	return nil
}

// FrameworkVerbs is the help for the framework's verbs in the monorepo the
// harness runs in, those a bot may run when turn is set: "" outside a
// monorepo, or when its framework cannot be read.
func FrameworkVerbs(turn bool) string {
	cwd, _ := os.Getwd()
	root, err := repo.FindRoot(cwd)
	if err != nil {
		return ""
	}
	cfg, err := repo.Load(root)
	if err != nil {
		return ""
	}
	m, err := contract.LoadManifest(filepath.Join(root, cfg.Framework))
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, v := range m.Names() {
		if turn && !m.InTurn(v) {
			continue
		}
		help := m.Verbs[v].Help
		if help == "" {
			help = "the framework's " + v
		}
		fmt.Fprintf(&b, "  harness %-19s %s\n", v+" [args]", help)
	}
	if b.Len() == 0 {
		return ""
	}
	return "The framework's verbs, in this monorepo:\n\n" + b.String() +
		"\nThey take -p <project> first to pick a project, and -p framework for the\nframework itself; the args after it are the framework's.\n"
}
