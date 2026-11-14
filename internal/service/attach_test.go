package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachments(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	wt := filepath.Join(root, "wt")
	write(t, filepath.Join(wt, "rec/shot-1.png"), "png")
	write(t, filepath.Join(wt, "rec/video.webm"), "webm")
	write(t, filepath.Join(wt, "notes.txt"), "text")
	write(t, filepath.Join(root, "secret.png"), "png")
	// A link inside the worktree to a file outside it is outside.
	os.Symlink(filepath.Join(root, "secret.png"), filepath.Join(wt, "rec/link.png"))

	body := "Before:\n\n![the greeting](shot-1.png \"title\")\n\n![](<video.webm>)\n\n" +
		"Again ![the greeting](shot-1.png), a [link](shot-1.png) and ![remote](https://example.com/a.png)."
	out, files, err := attachments(body, filepath.Join(wt, "rec"), wt)
	if err != nil {
		t.Fatal(err)
	}
	shot, video := filepath.Join(wt, "rec/shot-1.png"), filepath.Join(wt, "rec/video.webm")
	if len(files) != 2 || files[0] != shot || files[1] != video {
		t.Errorf("files: %q", files)
	}
	for _, want := range []string{"![the greeting](" + shot + " \"title\")", "![](" + video + ")",
		"Again ![the greeting](" + shot + ")", "[link](shot-1.png)", "![remote](https://example.com/a.png)"} {
		if !strings.Contains(out, want) {
			t.Errorf("body lacks %q:\n%s", want, out)
		}
	}

	for ref, want := range map[string]string{
		"![](missing.png)":             "no such file",
		"![](../secret.png)":           "outside your worktree",
		"![](rec/link.png)":            "outside your worktree",
		"![](" + root + "/secret.png)": "outside your worktree",
		"![](notes.txt)":               "only these can be shown",
		"![](rec)":                     "not a file",
	} {
		if _, _, err := attachments(ref, wt, wt); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", ref, err, want)
		}
	}
}
