package cli

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/michaelckearney/harness/contract"
	"github.com/michaelckearney/harness/internal/version"
)

//go:embed templates
var templates embed.FS

const initUsage = `usage: harness init -repo <owner/name> [-from <framework folder>]

Makes this folder a monorepo: harness.yaml, AGENTS.md,,
projects/, and the pipelines in .github/. Files already there are left
alone.

  -repo <owner/name>    the GitHub repository it will be
  -from <folder>        copy a framework from another monorepo into
                        framework/, to start from; the copy is this
                        monorepo's from then on
`

// Init runs `harness init`.
func Init(args []string, out io.Writer) error {
	fl := flag.NewFlagSet("init", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	var repoName, from string
	fl.StringVar(&repoName, "repo", "", "")
	fl.StringVar(&from, "from", "", "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, initUsage)
			return nil
		}
		return fail("harness init: %v\n\n%s", err, initUsage)
	}
	owner, name, ok := strings.Cut(repoName, "/")
	if !ok || owner == "" || name == "" {
		return fail("harness init needs -repo <owner/name>\n\n%s", initUsage)
	}
	root, _ := os.Getwd()
	v := version.Get()
	if v == "dev" {
		v = "main"
	}
	data := map[string]string{"Repo": repoName, "Owner": owner, "Name": name, "Version": v}
	err := fs.WalkDir(templates, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".tmpl")
		if strings.HasPrefix(rel, "github/") {
			rel = "." + rel
		}
		dst := filepath.Join(root, rel)
		if _, err := os.Stat(dst); err == nil {
			fmt.Fprintf(out, "kept    %s\n", rel)
			return nil
		}
		raw, err := templates.ReadFile(p)
		if err != nil {
			return err
		}
		t, err := template.New(rel).Parse(string(raw))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := t.Execute(f, data); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote   %s\n", rel)
		return nil
	})
	if err != nil {
		return err
	}
	for _, d := range []string{"projects"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return err
		}
	}
	if from != "" {
		if _, err := contract.LoadManifest(from); err != nil {
			return fail("-from %s is not a framework: %v", from, err)
		}
		dst := filepath.Join(root, "framework")
		if _, err := os.Stat(dst); err == nil {
			return fail("framework/ is there already")
		}
		// git's own copy leaves out what the framework's repository ignores,
		// such as caches and builds.
		if err := copyTracked(from, dst); err != nil {
			return err
		}
		fmt.Fprintf(out, "copied  %s to framework/\n", from)
	}
	return nil
}

// copyTracked copies a folder's files that git does not ignore.
func copyTracked(src, dst string) error {
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = src
	out, err := cmd.Output()
	if err != nil {
		return fail("-from must be a folder in a git repository: %v", err)
	}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		from := filepath.Join(src, rel)
		info, err := os.Lstat(from)
		if err != nil {
			continue // deleted, not yet committed
		}
		to := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}
