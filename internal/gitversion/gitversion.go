// Package gitversion works out a project's version from git: the latest
// release tag, and the titles of the pull requests merged since.
//
// Pull requests are squash-merged, so each is one commit on the default
// branch, and its title is a conventional commit:
//
//	feat!: or a BREAKING CHANGE: footer   a breaking change
//	feat:                                  a feature
//	fix:, perf:                            a fix
//	build:, chore:, ci:, docs:,            nothing to release
//	refactor:, revert:, style:, test:
//
// Before 1.0.0 a breaking change makes a minor release and a feature or a
// fix a patch release; from 1.0.0, a breaking change makes a major release,
// a feature a minor one, and a fix a patch.
//
// The tag is the version's only record. Nothing writes the version into the
// repository, so no release commit lands on the default branch.
package gitversion

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Version is MAJOR.MINOR.PATCH.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Tag is the version's git tag, such as v0.5.0.
func (v Version) Tag() string { return "v" + v.String() }

// Less reports whether v comes before w.
func (v Version) Less(w Version) bool {
	if v.Major != w.Major {
		return v.Major < w.Major
	}
	if v.Minor != w.Minor {
		return v.Minor < w.Minor
	}
	return v.Patch < w.Patch
}

var tagPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// ParseTag reads a tag such as v0.5.0.
func ParseTag(tag string) (Version, bool) {
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	return v, true
}

// Bump is how much a change moves the version.
type Bump int

const (
	None Bump = iota
	Patch
	Feature
	Breaking
)

func (b Bump) String() string {
	return [...]string{"nothing", "a fix", "a feature", "a breaking change"}[b]
}

// apply moves v by b.
func (v Version) apply(b Bump) Version {
	switch {
	case b == Breaking && v.Major == 0, b == Feature && v.Major > 0:
		return Version{v.Major, v.Minor + 1, 0}
	case b == Breaking:
		return Version{v.Major + 1, 0, 0}
	case b == None:
		return v
	default:
		return Version{v.Major, v.Minor, v.Patch + 1}
	}
}

// types are the conventional commit types, and what each releases.
var types = map[string]Bump{
	"feat": Feature, "fix": Patch, "perf": Patch,
	"build": None, "chore": None, "ci": None, "docs": None,
	"refactor": None, "revert": None, "style": None, "test": None,
}

var title = regexp.MustCompile(`^([a-z]+)(\([^()]+\))?(!)?: \S`)

var breakingFooter = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE: `)

// Change is one commit on the default branch since the last release.
type Change struct {
	SHA     string
	Subject string
	Type    string // "" when the subject is not a conventional commit
	Bump    Bump
}

// classify reads a commit's subject and body. ok is false for a subject
// that is not a conventional commit.
func classify(subject, body string) (typ string, b Bump, ok bool) {
	m := title.FindStringSubmatch(subject)
	if m == nil {
		return "", None, false
	}
	b, ok = types[m[1]]
	if !ok {
		return "", None, false
	}
	if m[3] == "!" || breakingFooter.MatchString(body) {
		b = Breaking
	}
	return m[1], b, true
}

// Conventional reports whether a pull request title is a conventional
// commit that the harness can read, such as "feat: add a thing".
func Conventional(subject string) bool {
	_, _, ok := classify(subject, "")
	return ok
}

// Series is what one line of releases is in a monorepo: its tags start with
// Prefix, such as harbor/v0.2.0, and only the commits that change Path count
// for it. The zero Series is the whole repository, tagged v0.2.0.
type Series struct {
	Prefix string
	Path   string
}

// For is the series of a project, or the whole repository for "".
func For(project, path string) Series {
	if project == "" {
		return Series{}
	}
	return Series{Prefix: project + "/", Path: path}
}

// Tag is v's tag in the series.
func (s Series) Tag(v Version) string { return s.Prefix + v.Tag() }

// Plan is what a release from HEAD would be.
type Plan struct {
	// Last is the latest release, or nil before the first.
	Last *Version
	// Next is the version to release.
	Next Version
	// Bump is the largest change since Last. None means there is nothing to
	// release, and Next is Last.
	Bump    Bump
	Changes []Change
	// Unreadable are the changes whose subject is not a conventional commit.
	Unreadable []Change
	Series     Series
}

// Releases reports whether there is something to release.
func (p Plan) Releases() bool { return p.Bump != None }

// Error is a problem the user can fix.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// Calculate plans a release from HEAD of the repository dir is in.
//
// The first release, with no tag before it, is 0.1.0, whatever the history
// says: commits from before the project used conventional commits need not
// be read.
func Calculate(ctx context.Context, dir string, series Series) (Plan, error) {
	if _, err := git(ctx, dir, "rev-parse", "HEAD"); err != nil {
		return Plan{}, fail("the harness takes the version from git, and %s is not in a git repository with a commit", dir)
	}
	if out, _ := git(ctx, dir, "rev-parse", "--is-shallow-repository"); strings.TrimSpace(out) == "true" {
		return Plan{}, fail("this clone is shallow, so the harness cannot see the tags and commits that give the version; clone the whole history (in GitHub Actions: actions/checkout with fetch-depth: 0)")
	}
	last, err := latest(ctx, dir, series)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Series: series}
	if last == nil {
		p.Next, p.Bump = Version{0, 1, 0}, Feature
		return p, nil
	}
	p.Last = last
	// First parent only: the default branch's own commits, one per merged
	// pull request, not the commits on the branches merged into it.
	logArgs := []string{"log", "--first-parent", "--format=%H%x00%s%x00%b%x1e", series.Tag(*last) + "..HEAD"}
	if series.Path != "" {
		logArgs = append(logArgs, "--", series.Path)
	}
	out, err := git(ctx, dir, logArgs...)
	if err != nil {
		return Plan{}, err
	}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x00", 3)
		if len(f) < 3 {
			continue
		}
		c := Change{SHA: f[0], Subject: f[1]}
		var ok bool
		c.Type, c.Bump, ok = classify(f[1], f[2])
		if !ok {
			p.Unreadable = append(p.Unreadable, c)
			continue
		}
		p.Changes = append(p.Changes, c)
		p.Bump = max(p.Bump, c.Bump)
	}
	p.Next = last.apply(p.Bump)
	return p, nil
}

// latest is the highest release tag that HEAD contains, or nil.
func latest(ctx context.Context, dir string, series Series) (*Version, error) {
	out, err := git(ctx, dir, "tag", "--merged", "HEAD", "--list", series.Prefix+"v*")
	if err != nil {
		return nil, err
	}
	var best *Version
	for _, t := range strings.Fields(out) {
		if v, ok := ParseTag(strings.TrimPrefix(t, series.Prefix)); ok && (best == nil || best.Less(v)) {
			best = &v
		}
	}
	return best, nil
}

// Strict refuses a plan with a change whose subject is not a conventional
// commit: its pull request title was not checked, and what it releases is
// a guess.
func (p Plan) Strict() error {
	if len(p.Unreadable) == 0 {
		return nil
	}
	var lines []string
	for _, c := range p.Unreadable {
		lines = append(lines, fmt.Sprintf("  %.7s %s", c.SHA, c.Subject))
	}
	return fail("these commits since %s are not conventional commits, such as \"fix: …\" or \"feat: …\", so the harness cannot tell what they release:\n%s\nrelease them with an explicit version, such as harness release -version=%s",
		p.Series.Tag(*p.Last), strings.Join(lines, "\n"), p.Last.apply(Patch))
}

// Build is the version a build of HEAD gets.
//
//	0.5.0                    HEAD is the commit tagged v0.5.0, with no changes
//	0.5.0-dev.abc1234        the next version, then the commit
//	0.5.0-dev.abc1234.dirty  with changes not committed
//
// A branch's own commits need not be conventional commits; only the pull
// request title is, once it is merged. So Build never fails on one, and the
// next version is at least a patch release.
func Build(ctx context.Context, dir string, series Series) (string, error) {
	p, err := Calculate(ctx, dir, series)
	if err != nil {
		return "", err
	}
	sha, err := git(ctx, dir, "rev-parse", "--short=7", "HEAD")
	if err != nil {
		return "", err
	}
	status, err := git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	dirty := strings.TrimSpace(status) != ""
	if p.Last != nil && !dirty {
		if tags, _ := git(ctx, dir, "tag", "--points-at", "HEAD"); slices.Contains(strings.Fields(tags), series.Tag(*p.Last)) {
			return p.Last.String(), nil
		}
	}
	next := p.Next
	if p.Last != nil && p.Bump == None {
		next = p.Last.apply(Patch)
	}
	v := next.String() + "-dev." + strings.TrimSpace(sha)
	if dirty {
		v += ".dirty"
	}
	return v, nil
}

// Core is a version's MAJOR.MINOR.PATCH, without what follows a "-": what
// macOS and Windows take as an app's version.
func Core(version string) string {
	core, _, _ := strings.Cut(version, "-")
	return core
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}
