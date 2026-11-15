package discord

import (
	"regexp"
	"strings"
)

// Limit is how much text an embed's description holds.
const Limit = 4096

var (
	comment = regexp.MustCompile(`(?s)<!--.*?-->`)
	image   = regexp.MustCompile(`!\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	imgTag  = regexp.MustCompile(`(?i)<img\b[^>]*\bsrc\s*=\s*["']([^"']+)["'][^>]*>`)
	video   = regexp.MustCompile(`(?m)^[ \t]*(https://github\.com/user-attachments/assets/\S+)[ \t]*$`)
	summary = regexp.MustCompile(`(?is)<summary>(.*?)</summary>`)
	tags    = regexp.MustCompile(`(?i)</?(details|summary|p|div|br|sub|sup)\b[^>]*>`)
	heading = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	task    = regexp.MustCompile(`(?m)^(\s*[-*])\s+\[([ xX])\]\s`)
	blank   = regexp.MustCompile(`\n{3,}`)
)

// Translate turns a GitHub description into what an embed shows: Discord's
// smaller Markdown, with the files it shows, images and videos, taken out to
// be attached instead, in order.
func Translate(body string) (text string, files []string) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = comment.ReplaceAllString(body, "")
	take := func(re *regexp.Regexp) {
		body = re.ReplaceAllStringFunc(body, func(m string) string {
			files = append(files, re.FindStringSubmatch(m)[1])
			return ""
		})
	}
	take(image)
	take(imgTag)
	take(video)
	body = summary.ReplaceAllString(body, "**$1**")
	body = tags.ReplaceAllString(body, "")
	body = heading.ReplaceAllString(body, "**$1**")
	body = task.ReplaceAllStringFunc(body, func(m string) string {
		sub := task.FindStringSubmatch(m)
		if sub[2] == " " {
			return sub[1] + " ☐ "
		}
		return sub[1] + " ☑ "
	})
	body = tables(body)
	body = blank.ReplaceAllString(strings.TrimSpace(body), "\n\n")
	return body, files
}

// tables puts GitHub's tables, which Discord does not draw, in code blocks,
// where they line up. Lines in code blocks are left alone.
func tables(body string) string {
	var out []string
	code, table := false, false
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			code = !code
		}
		row := !code && strings.HasPrefix(strings.TrimSpace(l), "|")
		if row && !table {
			out = append(out, "```")
		}
		if !row && table {
			out = append(out, "```")
		}
		table = row
		out = append(out, l)
	}
	if table {
		out = append(out, "```")
	}
	return strings.Join(out, "\n")
}

// Split cuts text into what fits an embed, and the rest, in pieces that
// fit a message, cut at lines where it can be.
func Split(text string, first, rest int) []string {
	var out []string
	n := first
	for len(text) > n {
		cut := strings.LastIndex(text[:n], "\n")
		if cut < n/2 {
			cut = n
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
		n = rest
	}
	return append(out, text)
}
