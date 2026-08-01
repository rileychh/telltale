package github

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reCodeBlock  = regexp.MustCompile("(?s)```[a-zA-Z]*\n?.*?```")
	reInline     = regexp.MustCompile("`[^`]+`")
	reImage      = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	reLinkedImg  = regexp.MustCompile(`\[!\[([^\]]*)\]\([^)]+\)\]\(([^)]+)\)`)
	reHTMLImg    = regexp.MustCompile(`(?i)<img\s[^>]*src=["']([^"']+)["'][^>]*/?>`)
	reHTMLImgAlt = regexp.MustCompile(`(?i)alt=["']([^"']*)["']`)
	reMdLink     = regexp.MustCompile(`!?\[[^\]]*\]\([^)]*\)`)
	reBareURL    = regexp.MustCompile(`https?://\S+`)
	reIssueRef   = regexp.MustCompile(`(?:^|[^&\w])#(\d+)\b`)
	reCommitSHA  = regexp.MustCompile(`\b([0-9a-f]{7,40})\b`)

	// HTML comments are invisible on GitHub, so drop them rather than escaping
	// them into visible text.
	reHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)

	// Tags Telegram renders in a rich message. Anything else is escaped so it
	// survives as literal text: Telegram silently discards unknown tags, which
	// would eat prose like "returns List<String>" down to "returns List".
	// Structural tags Telegram ignores (thead, br) are listed too — having them
	// dropped is better than showing them escaped.
	reSupportedTag = regexp.MustCompile(`(?i)^</?(?:a|b|strong|i|em|u|ins|s|strike|del|code|mark|sub|sup|h[1-6]|p|pre|footer|hr|br|ul|ol|li|input|blockquote|aside|cite|img|video|audio|figure|figcaption|table|thead|tbody|tfoot|tr|td|th|caption|details|summary|tg-spoiler|tg-reference|tg-emoji|tg-time|tg-math|tg-math-block|tg-map|tg-collage|tg-slideshow)(?:\s[^>]*)?/?>`)

	// Block-level HTML inside which Telegram does not parse Markdown, so an
	// image demoted to a link must be written as an HTML anchor rather than
	// Markdown. <details> is deliberately absent: it does parse Markdown.
	reHTMLBlockOpen  = regexp.MustCompile(`(?i)<(table|ul|ol|blockquote|figure|aside|p)(?:\s[^>]*)?>`)
	reHTMLBlockClose = regexp.MustCompile(`(?i)</(table|ul|ol|blockquote|figure|aside|p)\s*>`)
)

// maxMedia is Telegram's per-rich-message limit on media attachments.
// Images beyond it are rendered as links instead.
const maxMedia = 50

// prepareMarkdown adapts GitHub-flavored Markdown for Telegram's rich message
// Markdown, which accepts GFM largely as-is. Only two things need rewriting:
// GitHub autolinks, which depend on repo context Telegram doesn't have, and
// images, which Telegram renders as media only when they stand alone as their
// own block.
func prepareMarkdown(md, repo string) string {
	// Strip zero-width space HTML entities (used by Renovate to suppress @mentions)
	s := strings.ReplaceAll(md, "&#8203;", "")

	// Protect code so the rewrites below don't fire inside it.
	var code []string
	protect := func(re *regexp.Regexp, s string) string {
		return re.ReplaceAllStringFunc(s, func(match string) string {
			placeholder := fmt.Sprintf("\x00CODE%d\x00", len(code))
			code = append(code, match)
			return placeholder
		})
	}
	s = protect(reCodeBlock, s)
	s = protect(reInline, s)

	s = reHTMLComment.ReplaceAllString(s, "")
	s = escapeUnsupportedTags(s)

	s = rewriteImages(s)

	// Protect existing links and bare URLs. Their targets routinely contain
	// long hex runs (GitHub attachment URLs, for one) that the commit-SHA
	// autolink below would otherwise rewrite mid-URL.
	s = protect(reMdLink, s)
	s = protect(reBareURL, s)

	// GitHub autolinks: Telegram has no repo context, and a bare #123 would
	// otherwise be detected as a hashtag.
	s = reIssueRef.ReplaceAllStringFunc(s, func(match string) string {
		parts := reIssueRef.FindStringSubmatch(match)
		prefix := match[:len(match)-len("#"+parts[1])]
		return fmt.Sprintf("%s[#%s](https://github.com/%s/issues/%s)", prefix, parts[1], repo, parts[1])
	})
	s = reCommitSHA.ReplaceAllStringFunc(s, func(match string) string {
		return fmt.Sprintf("[%s](https://github.com/%s/commit/%s)", match[:7], repo, match)
	})

	for i, block := range code {
		s = strings.Replace(s, fmt.Sprintf("\x00CODE%d\x00", i), block, 1)
	}

	return strings.TrimSpace(s)
}

// rewriteImages decides which images become Telegram media blocks. Telegram
// renders media only when it is a block of its own, so an image is kept as
// media only when it is the entire content of its line; anything inline or
// wrapped in a link becomes a plain link instead. SVGs are always linked, as
// Telegram has no media type for them.
func rewriteImages(s string) string {
	mediaCount := 0
	blockDepth := 0

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		opens := len(reHTMLBlockOpen.FindAllString(line, -1))
		closes := len(reHTMLBlockClose.FindAllString(line, -1))
		// A line that opens a block counts as inside it, so a single-line
		// <table>…<img>…</table> is handled like the multi-line form.
		inHTMLBlock := blockDepth > 0 || opens > 0
		blockDepth += opens - closes
		if blockDepth < 0 {
			blockDepth = 0
		}

		// Inside block HTML, Markdown isn't parsed, so no image can be media
		// and every link must be written as an HTML anchor.
		if inHTMLBlock {
			lines[i] = reHTMLImg.ReplaceAllStringFunc(line, func(match string) string {
				return imageAnchor(altOf(match), reHTMLImg.FindStringSubmatch(match)[1])
			})
			lines[i] = reImage.ReplaceAllStringFunc(lines[i], func(match string) string {
				parts := reImage.FindStringSubmatch(match)
				return imageAnchor(parts[1], parts[2])
			})
			continue
		}

		// An image alone on its line stays media, as long as there is budget
		// left and Telegram can render the format.
		if m := reImage.FindStringSubmatch(trimmed); m != nil && m[0] == trimmed {
			if mediaCount < maxMedia && isRenderableMedia(m[2]) {
				mediaCount++
				continue
			}
			lines[i] = imageLink(m[1], m[2])
			continue
		}
		if m := reHTMLImg.FindStringSubmatch(trimmed); m != nil && m[0] == trimmed {
			alt := altOf(trimmed)
			if mediaCount < maxMedia && isRenderableMedia(m[1]) {
				lines[i] = fmt.Sprintf("![%s](%s)", alt, m[1])
				mediaCount++
				continue
			}
			lines[i] = imageLink(alt, m[1])
			continue
		}

		// Any remaining image on this line is inline or wrapped in a link, so
		// it can't be media regardless of its format. A linked image collapses
		// to a single link pointing at the link target, not the image, so
		// badges read as one link rather than a broken nested one.
		lines[i] = reLinkedImg.ReplaceAllStringFunc(line, func(match string) string {
			parts := reLinkedImg.FindStringSubmatch(match)
			return imageLink(parts[1], parts[2])
		})
		lines[i] = reImage.ReplaceAllStringFunc(lines[i], func(match string) string {
			parts := reImage.FindStringSubmatch(match)
			return imageLink(parts[1], parts[2])
		})
		lines[i] = reHTMLImg.ReplaceAllStringFunc(lines[i], func(match string) string {
			return imageLink(altOf(match), reHTMLImg.FindStringSubmatch(match)[1])
		})
	}

	return strings.Join(lines, "\n")
}

// imageLink renders an image that can't be a media block as a Markdown link.
func imageLink(alt, url string) string {
	if alt == "" {
		alt = "Image"
	}
	return fmt.Sprintf("[%s](%s)", alt, url)
}

// escapeUnsupportedTags escapes every "<" that doesn't begin a tag Telegram
// supports. Telegram drops unknown tags along with nothing else, so an
// unescaped "<T>" or "<String>" in prose disappears from the message entirely.
func escapeUnsupportedTags(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			b.WriteByte(s[i])
			continue
		}
		if loc := reSupportedTag.FindStringIndex(s[i:]); loc != nil {
			b.WriteString(s[i : i+loc[1]])
			i += loc[1] - 1
			continue
		}
		b.WriteString("&lt;")
	}
	return b.String()
}

// imageAnchor is imageLink for contexts where Markdown isn't parsed.
func imageAnchor(alt, url string) string {
	if alt == "" {
		alt = "Image"
	}
	attr := strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;", ">", "&gt;")
	text := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return fmt.Sprintf(`<a href="%s">%s</a>`, attr.Replace(url), text.Replace(alt))
}

// altOf extracts an <img> tag's alt attribute, or "" if it has none.
func altOf(imgTag string) string {
	if a := reHTMLImgAlt.FindStringSubmatch(imgTag); a != nil {
		return a[1]
	}
	return ""
}

// isRenderableMedia reports whether Telegram can display the URL as media.
// Telegram picks the media type from the MIME type and URL, and has none for SVG.
func isRenderableMedia(url string) bool {
	return !strings.HasSuffix(strings.ToLower(url), ".svg")
}

// escapeMarkdown escapes text interpolated into a rich Markdown message.
// Rich Markdown parses HTML, so angle brackets and ampersands become entities;
// the rest are Markdown and Telegram-extension metacharacters.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`\`, `\\`,
		"*", `\*`,
		"_", `\_`,
		"~", `\~`,
		"`", "\\`",
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"#", `\#`,
		"|", `\|`,
		"=", `\=`,
		"$", `\$`,
		"!", `\!`,
	)
	return r.Replace(s)
}
