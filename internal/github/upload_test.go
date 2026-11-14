package github

import "testing"

func TestLinkFile(t *testing.T) {
	body := "![shot](/w/a.png \"t\")\n\n![the recording](</w/video.webm>)\n\nnot ![](/w/b.png)"
	body = linkFile(body, "/w/a.png", "https://x/1")
	body = linkFile(body, "/w/video.webm", "https://x/2")
	want := "![shot](https://x/1 \"t\")\n\nhttps://x/2\n\nnot ![](/w/b.png)"
	if body != want {
		t.Errorf("got\n%s\nwant\n%s", body, want)
	}
	for api, want := range map[string]string{
		"https://api.github.com":   "https://uploads.github.com",
		"https://api.acme.ghe.com": "https://uploads.acme.ghe.com",
		"http://127.0.0.1:4000":    "http://127.0.0.1:4000",
	} {
		if got := (&Client{API: api}).uploads(); got != want {
			t.Errorf("%s: %s, want %s", api, got, want)
		}
	}
}
