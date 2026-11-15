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
