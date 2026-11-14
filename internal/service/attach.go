package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// imageRef is a Markdown image, ![alt](target "title"), with the target in
// the second group, with or without angle brackets.
var imageRef = regexp.MustCompile(`!\[[^\]]*\]\(\s*(<[^>]*>|[^)\s]+)(\s+"[^"]*")?\s*\)`)

// mediaTypes are the files GitHub takes as attachments that show in place:
// images inline, videos as a player.
var mediaTypes = []string{".png", ".gif", ".jpg", ".jpeg", ".svg", ".webp", ".mp4", ".mov", ".webm"}

// attachments finds the local files a body's images name, so they can be
// uploaded, and rewrites each to its absolute path, which is what gh then
// replaces with the uploaded URL. A relative path is from dir, where the
// body came from.
//
// Only files inside the worktree: text the bot wrote can have come from a
// site or a file it read, and without the limit "show me ~/.ssh/id_ed25519"
// would upload the key for anyone who can read the issue.
func attachments(body, dir, worktree string) (string, []string, error) {
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return "", nil, err
	}
	var files []string
	var problems []string
	out := imageRef.ReplaceAllStringFunc(body, func(ref string) string {
		m := imageRef.FindStringSubmatchIndex(ref)
		target := ref[m[2]:m[3]]
		path := strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
		if path == "" || strings.Contains(path, "://") || strings.HasPrefix(path, "data:") || strings.HasPrefix(path, "#") {
			return ref
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: there is no such file", target))
			return ref
		}
		if rel, err := filepath.Rel(root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			problems = append(problems, fmt.Sprintf("%s: it is outside your worktree; only files in it can be shown", target))
			return ref
		}
		if info, err := os.Stat(real); err != nil || !info.Mode().IsRegular() {
			problems = append(problems, fmt.Sprintf("%s: it is not a file", target))
			return ref
		}
		if !slices.Contains(mediaTypes, strings.ToLower(filepath.Ext(real))) {
			problems = append(problems, fmt.Sprintf("%s: only these can be shown: %s", target, strings.Join(mediaTypes, " ")))
			return ref
		}
		if strings.ContainsAny(real, " \t<>()") {
			problems = append(problems, fmt.Sprintf("%s: rename it without spaces or brackets", target))
			return ref
		}
		if !slices.Contains(files, real) {
			files = append(files, real)
		}
		return ref[:m[2]] + real + ref[m[3]:]
	})
	if len(problems) > 0 {
		return "", nil, fixable("these images cannot be shown:\n" + indent(strings.Join(problems, "\n")))
	}
	return out, files, nil
}
