package github

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reCodeBlock = regexp.MustCompile("(?s)```[a-zA-Z]*\n?(.*?)```")
	reInline    = regexp.MustCompile("`([^`]+)`")
	reBold      = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reItalic    = regexp.MustCompile(`(?:^|[^*])\*([^*]+?)\*(?:[^*]|$)`)
	reLink      = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reImage     = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)
	reHeading   = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reBlockquote = regexp.MustCompile(`(?m)^>\s?(.*)$`)
	reCheckbox  = regexp.MustCompile(`(?m)^- \[([ xX])\] `)
	reIssueRef  = regexp.MustCompile(`(?:^|[^&\w])#(\d+)\b`)
	reCommitSHA = regexp.MustCompile(`\b([0-9a-f]{7,40})\b`)
)

// mdToTelegramHTML converts GitHub-flavored Markdown to Telegram-compatible HTML.
func mdToTelegramHTML(md, repo string) string {
	// Protect code blocks by replacing with placeholders
	var codeBlocks []string
	s := reCodeBlock.ReplaceAllStringFunc(md, func(match string) string {
		inner := reCodeBlock.FindStringSubmatch(match)[1]
		placeholder := fmt.Sprintf("\x00CODEBLOCK%d\x00", len(codeBlocks))
		codeBlocks = append(codeBlocks, "<pre>"+escapeHTML(strings.TrimSpace(inner))+"</pre>")
		return placeholder
	})

	// Protect inline code
	var inlineCodes []string
	s = reInline.ReplaceAllStringFunc(s, func(match string) string {
		inner := reInline.FindStringSubmatch(match)[1]
		placeholder := fmt.Sprintf("\x00INLINE%d\x00", len(inlineCodes))
		inlineCodes = append(inlineCodes, "<code>"+escapeHTML(inner)+"</code>")
		return placeholder
	})

	// Protect blockquotes before escaping (> would become &gt;)
	var blockquotes []string
	s = groupBlockquotesWithPlaceholders(s, &blockquotes)

	// Strip images (before link processing)
	s = reImage.ReplaceAllString(s, "$1")

	// Convert links before escaping (URLs contain & etc.)
	var links []string
	s = reLink.ReplaceAllStringFunc(s, func(match string) string {
		parts := reLink.FindStringSubmatch(match)
		placeholder := fmt.Sprintf("\x00LINK%d\x00", len(links))
		links = append(links, fmt.Sprintf(`<a href="%s">%s</a>`, parts[2], escapeHTML(parts[1])))
		return placeholder
	})

	// Escape HTML in remaining text
	s = escapeHTML(s)

	// Convert Markdown formatting
	s = reHeading.ReplaceAllString(s, "<b>$1</b>")
	s = reBold.ReplaceAllString(s, "<b>$1</b>")
	s = reItalic.ReplaceAllStringFunc(s, func(match string) string {
		inner := reItalic.FindStringSubmatch(match)[1]
		prefix := ""
		suffix := ""
		if match[0] != '*' {
			prefix = string(match[0])
		}
		if match[len(match)-1] != '*' {
			suffix = string(match[len(match)-1])
		}
		return prefix + "<i>" + inner + "</i>" + suffix
	})
	s = reCheckbox.ReplaceAllStringFunc(s, func(match string) string {
		parts := reCheckbox.FindStringSubmatch(match)
		if parts[1] == " " {
			return "☐ "
		}
		return "☑ "
	})

	// GitHub autolinks
	s = reIssueRef.ReplaceAllStringFunc(s, func(match string) string {
		parts := reIssueRef.FindStringSubmatch(match)
		prefix := match[:len(match)-len("#"+parts[1])]
		return fmt.Sprintf(`%s<a href="https://github.com/%s/issues/%s">#%s</a>`, prefix, repo, parts[1], parts[1])
	})
	s = reCommitSHA.ReplaceAllStringFunc(s, func(match string) string {
		return fmt.Sprintf(`<a href="https://github.com/%s/commit/%s">%s</a>`, repo, match, match[:7])
	})

	// Restore placeholders
	for i, block := range codeBlocks {
		s = strings.Replace(s, fmt.Sprintf("\x00CODEBLOCK%d\x00", i), block, 1)
	}
	for i, code := range inlineCodes {
		s = strings.Replace(s, fmt.Sprintf("\x00INLINE%d\x00", i), code, 1)
	}
	for i, link := range links {
		s = strings.Replace(s, fmt.Sprintf("\x00LINK%d\x00", i), link, 1)
	}
	for i, bq := range blockquotes {
		s = strings.Replace(s, fmt.Sprintf("\x00BLOCKQUOTE%d\x00", i), bq, 1)
	}

	return strings.TrimSpace(s)
}

// groupBlockquotesWithPlaceholders extracts consecutive `> ` lines, converts them to
// <blockquote> HTML, and replaces them with placeholders to survive escapeHTML.
func groupBlockquotesWithPlaceholders(s string, out *[]string) string {
	lines := strings.Split(s, "\n")
	var result []string
	var quoteBlock []string

	flush := func() {
		if len(quoteBlock) > 0 {
			placeholder := fmt.Sprintf("\x00BLOCKQUOTE%d\x00", len(*out))
			*out = append(*out, "<blockquote>"+escapeHTML(strings.Join(quoteBlock, "\n"))+"</blockquote>")
			result = append(result, placeholder)
			quoteBlock = nil
		}
	}

	for _, line := range lines {
		if reBlockquote.MatchString(line) {
			inner := reBlockquote.FindStringSubmatch(line)[1]
			quoteBlock = append(quoteBlock, inner)
		} else {
			flush()
			result = append(result, line)
		}
	}
	flush()

	return strings.Join(result, "\n")
}

func escapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

