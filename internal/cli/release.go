package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/michaelckearney/harness/contract"
	"github.com/michaelckearney/harness/internal/dispatch"
	"github.com/michaelckearney/harness/internal/gitversion"
	"github.com/michaelckearney/harness/internal/repo"
)

const releaseUsage = `usage: harness release [-p <project>] [-plan | -notes] [-version=<version>]

For the default branch's pipeline. Each project has its own releases,
tagged <project>/v<version>, such as harbor/v0.2.0. A project's version
comes from the titles of the pull requests merged since its last tag that
changed its folder: feat a minor release, fix a patch, feat! a breaking
change (see the README).

Without -p, every project with something to release is released.

  -plan               print the version each project would release, or nothing
  -notes              print the release notes
  -version=<version>  release that version instead (needs -p)
`

// Release runs `harness release`: it builds each project that has
// something to release, with the framework's build verb, then tags it and
// publishes a GitHub release with the artifacts the build listed.
func Release(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var name, explicit string
	var plan, notes bool
	fs.StringVar(&name, "p", "", "")
	fs.BoolVar(&plan, "plan", false, "")
	fs.BoolVar(&notes, "notes", false, "")
	fs.StringVar(&explicit, "version", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, releaseUsage)
			return nil
		}
		return fail("harness release: %v\n\n%s", err, releaseUsage)
	}
	if explicit != "" && name == "" {
		return fail("harness release -version needs -p <project>")
	}
	cwd, _ := os.Getwd()
	root, err := repo.FindRoot(cwd)
	if err != nil {
		return &Error{err.Error()}
	}
	cfg, err := repo.Load(root)
	if err != nil {
		return &Error{err.Error()}
	}
	names := cfg.ProjectNames()
	if name != "" {
		if cfg.Projects[name] == nil {
			return fail("there is no project %q", name)
		}
		names = []string{name}
	}
	for _, n := range names {
		if err := releaseOne(ctx, root, cfg, n, explicit, plan, notes, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

func releaseOne(ctx context.Context, root string, cfg *repo.Config, name, explicit string, plan, notes bool, stdout, stderr io.Writer) error {
	t, err := cfg.ProjectTarget(name)
	if err != nil {
		return err
	}
	series := gitversion.For(name, t.ProjectPath)
	p, err := gitversion.Calculate(ctx, root, series)
	if err != nil {
		return &Error{err.Error()}
	}
	var v gitversion.Version
	switch {
	case explicit != "":
		var ok bool
		if v, ok = gitversion.ParseTag("v" + strings.TrimPrefix(explicit, "v")); !ok {
			return fail("-version %q is not a version; use three numbers, like 0.5.0", explicit)
		}
		if p.Last != nil && !p.Last.Less(v) {
			return fail("-version %s is not after %s's last release, %s", v, name, series.Tag(*p.Last))
		}
	case p.Strict() != nil:
		return &Error{p.Strict().Error()}
	case !p.Releases():
		if !plan && !notes {
			fmt.Fprintf(stderr, "harness: %s: nothing to release since %s\n", name, series.Tag(*p.Last))
		}
		return nil
	default:
		v = p.Next
	}
	tag := series.Tag(v)
	switch {
	case plan:
		fmt.Fprintf(stdout, "%s %s\n", name, v)
		return nil
	case notes:
		fmt.Fprintf(stdout, "# %s\n\n%s", tag, Notes(p))
		return nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return fail("harness release publishes with gh, which is not on PATH")
	}
	head, err := gitOut(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if status, err := gitOut(ctx, root, "status", "--porcelain"); err != nil {
		return err
	} else if status != "" {
		return fail("the checkout has changes that are not committed; a release is built from a commit alone:\n%s", status)
	}
	if _, err := gitOut(ctx, root, "rev-parse", "-q", "--verify", "refs/tags/"+tag); err == nil {
		return fail("%s is tagged already", tag)
	}
	fmt.Fprintf(stderr, "harness: releasing %s (%s)\n", tag, p.Bump)
	r, err := dispatch.Run(ctx, dispatch.Call{Root: root, Target: t, Verb: contract.Build, Version: v.String(), Cwd: root,
		Stdout: stderr, Stderr: stderr})
	if err != nil {
		return wrap(err)
	}
	var assets []string
	for _, a := range r.Artifacts {
		assets = append(assets, filepath.Join(root, t.ProjectPath, a))
	}
	cmd := exec.CommandContext(ctx, "gh", append([]string{"release", "create", tag,
		"--target", head, "--title", tag, "--notes-file", "-"}, assets...)...)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(Notes(p))
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		return fail("gh release create %s failed, so nothing is tagged: %v", tag, err)
	}
	fmt.Fprintf(stdout, "released %s\n", tag)
	return nil
}

// Notes are a release's notes: the merged pull requests' titles, by what
// they release.
func Notes(p gitversion.Plan) string {
	groups := []struct {
		title string
		bump  gitversion.Bump
	}{
		{"Breaking changes", gitversion.Breaking},
		{"Features", gitversion.Feature},
		{"Fixes", gitversion.Patch},
		{"Other changes", gitversion.None},
	}
	var b strings.Builder
	for _, g := range groups {
		var lines []string
		for _, c := range p.Changes {
			if c.Bump == g.bump {
				lines = append(lines, "- "+c.Subject)
			}
		}
		if g.bump == gitversion.None {
			for _, c := range p.Unreadable {
				lines = append(lines, "- "+c.Subject)
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "## %s\n\n%s\n\n", g.title, strings.Join(lines, "\n"))
		}
	}
	if b.Len() == 0 {
		if p.Last == nil {
			return "The first release.\n"
		}
		return "No pull requests merged since " + p.Series.Tag(*p.Last) + ".\n"
	}
	return b.String()
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
