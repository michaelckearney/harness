package discord

import (
	"fmt"
	"strings"
	"testing"
)

func TestTranslate(t *testing.T) {
	body := "<!-- the template -->\n## What\n\nAdds hello.\n\n![shot](https://github.com/user-attachments/assets/1)\n" +
		"<img width=\"300\" src=\"https://github.com/user-attachments/assets/2\" />\n\n" +
		"https://github.com/user-attachments/assets/3\n\n" +
		"<details><summary>Logs</summary>\n\nall fine\n</details>\n\n" +
		"- [ ] later\n- [x] done\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```\n| not a table |\n```\n"
	text, files := Translate(body)
	want := "**What**\n\nAdds hello.\n\n**Logs**\n\nall fine\n\n- ☐ later\n- ☑ done\n\n```\n| a | b |\n|---|---|\n| 1 | 2 |\n```\n\n```\n| not a table |\n```"
	if text != want {
		t.Errorf("text:\n%s\nwant:\n%s", text, want)
	}
	if got := fmt.Sprint(files); got != "[https://github.com/user-attachments/assets/1 https://github.com/user-attachments/assets/2 https://github.com/user-attachments/assets/3]" {
		t.Errorf("files: %s", got)
	}
}

func TestSplit(t *testing.T) {
	text := strings.Repeat("a line\n", 20)
	parts := Split(text, 30, 50)
	if len(parts[0]) > 30 || strings.Join(parts, "\n")+"\n" != text {
		t.Errorf("parts: %q", parts)
	}
	for _, p := range parts[1:] {
		if len(p) > 50 {
			t.Errorf("a part is %d long", len(p))
		}
	}
}

func TestCaptions(t *testing.T) {
	body := "![Before: the empty pane](https://x/1)\n![](https://x/2)\n<img alt=\"After: centred\" src=\"https://x/3\">"
	got := Captions(body)
	if len(got) != 2 || got["https://x/1"] != "Before: the empty pane" || got["https://x/3"] != "After: centred" {
		t.Errorf("captions: %q", got)
	}
}

func TestImagesShowUnderTheirCaptions(t *testing.T) {
	m := Message{Title: "#1", Color: 1, Files: []File{
		{Name: "1.png", ContentType: "image/png", Caption: "Before: it"},
		{Name: "2.mp4", ContentType: "video/mp4"},
		{Name: "3.png", ContentType: "image/png", Caption: strings.Repeat("a", 200)},
	}}
	e := m.embeds()
	if len(e) != 3 || e[1].Title != "Before: it" || e[1].Image.URL != "attachment://1.png" || e[2].Image.URL != "attachment://3.png" {
		t.Fatalf("embeds: %+v", e)
	}
	if n := len([]rune(e[2].Title)); n != captionLimit {
		t.Errorf("a long caption is %d long", n)
	}
}
