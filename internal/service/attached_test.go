package service

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachedFilesAreCopied(t *testing.T) {
	dir := t.TempDir()
	const id = "533917de-e51d-45ca-8acc-f8c924cfa617"
	const vid = "11111111-2222-3333-4444-555555555555"
	signed := "https://private-user-images.githubusercontent.com/1/2-" + id + ".png?jwt=abc&x=1"
	body := "Look:\n\n![the bug](https://github.com/user-attachments/assets/" + id + ")\n\n" +
		"https://github.com/user-attachments/assets/" + vid + "\n\nand [a link](https://example.com/a.png)."
	rendered := `<a href="` + strings.ReplaceAll(signed, "&", "&amp;") + `"><img src="` + strings.ReplaceAll(signed, "&", "&amp;") + `"></a>` +
		`<video src="https://github.com/user-attachments/assets/` + vid + `"></video>`

	var got []string
	get := func(u string) (*http.Response, error) {
		got = append(got, u)
		h := http.Header{}
		switch u {
		case signed:
			h.Set("Content-Type", "image/png")
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: h, Body: io.NopCloser(strings.NewReader("png"))}, nil
		case "https://github.com/user-attachments/assets/" + vid:
			h.Set("Content-Type", "video/mp4")
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: h, Body: io.NopCloser(strings.NewReader("mp4"))}, nil
		}
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader(""))}, nil
	}

	out, errs := localize(body, rendered, dir, get)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	png, mp4 := filepath.Join(dir, id+".png"), filepath.Join(dir, vid+".mp4")
	for _, want := range []string{"![the bug](" + png + ")", "\n" + mp4 + "\n", "[a link](https://example.com/a.png)"} {
		if !strings.Contains(out, want) {
			t.Errorf("body lacks %q:\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(png); string(b) != "png" {
		t.Errorf("png holds %q", b)
	}

	// Fetched once: the signed link has expired by the next poll.
	got = nil
	if again, _ := localize(body, "", dir, get); again != out || len(got) != 0 {
		t.Errorf("fetched again: %q\n%s", got, again)
	}

	// A file that cannot be fetched keeps its link.
	gone := "99999999-2222-3333-4444-555555555555"
	ref := "![x](https://github.com/user-attachments/assets/" + gone + ")"
	if out, errs := localize(ref, "", dir, get); out != ref || len(errs) != 1 {
		t.Errorf("%q, %v", out, errs)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".part-*")); len(left) > 0 {
		t.Errorf("left behind: %q", left)
	}
}
