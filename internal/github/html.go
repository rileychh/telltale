package github

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/rileychh/telltale/internal/tableimg"
)

var (
	reCodeBlock    = regexp.MustCompile("(?s)```[a-zA-Z]*\n?(.*?)```")
	reInline       = regexp.MustCompile("`([^`]+)`")
	reBold         = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reItalic       = regexp.MustCompile(`(?:^|[^*])\*([^*]+?)\*(?:[^*]|$)`)
	reLink         = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reImage        = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	reHTMLImg      = regexp.MustCompile(`(?i)<img\s[^>]*src=["']([^"']+)["'][^>]*/?>`)
	reHTMLImgAlt   = regexp.MustCompile(`(?i)alt=["']([^"']*)["']`)
	reHeading      = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reBlockquote   = regexp.MustCompile(`(?m)^>\s?(.*)$`)
	reCheckbox     = regexp.MustCompile(`(?m)^- \[([ xX*])\] `)
	reIssueRef     = regexp.MustCompile(`(?:^|[^&\w])#(\d+)\b`)
	reCommitSHA    = regexp.MustCompile(`\b([0-9a-f]{7,40})\b`)
	reCodeInLink   = regexp.MustCompile(`(<a [^>]*>)<code>([^<]*)</code>(</a>)`)
	reTableRow     = regexp.MustCompile(`^\|(.+)\|$`)
	reTableSep     = regexp.MustCompile(`^\|[-\s|:]+\|$`)
	reMediaMarker  = regexp.MustCompile(`\x00(IMG|TABLE)(\d+)\x00`)
)

// MediaRef is an image URL or a parsed table, in document order.
type MediaRef struct {
	URL   string          // URL-based image (mutually exclusive with Table)
	Table *tableimg.Table // rendered table (mutually exclusive with URL)
}

// mdToTelegramHTML converts GitHub-flavored Markdown to Telegram-compatible HTML.
// It returns the converted HTML and media references in document order.
func mdToTelegramHTML(md, repo string) (string, []MediaRef) {
	// Collect image URLs and tables by their placeholder index;
	// document order is determined later by scanning for placeholders.
	var imgURLByIdx []string          // IMG index → URL (empty if unsupported)
	var tableByIdx []tableimg.Table

	// Protect code blocks by replacing with placeholders
	var codeBlocks []string
	s := reCodeBlock.ReplaceAllStringFunc(md, func(match string) string {
		inner := reCodeBlock.FindStringSubmatch(match)[1]
		placeholder := fmt.Sprintf("\x00CODEBLOCK%d\x00", len(codeBlocks))
		codeBlocks = append(codeBlocks, "<pre>"+escapeHTML(strings.TrimSpace(inner))+"</pre>")
		return placeholder
	})

	// Extract markdown tables and replace with placeholders
	s = extractTablesWithPlaceholders(s, &tableByIdx)

	// Protect inline code
	var inlineCodes []string
	s = reInline.ReplaceAllStringFunc(s, func(match string) string {
		inner := reInline.FindStringSubmatch(match)[1]
		placeholder := fmt.Sprintf("\x00INLINE%d\x00", len(inlineCodes))
		inlineCodes = append(inlineCodes, "<code>"+escapeHTML(inner)+"</code>")
		return placeholder
	})

	// Process markdown images (before link processing)
	var imagePlaceholders []string
	s = reImage.ReplaceAllStringFunc(s, func(match string) string {
		parts := reImage.FindStringSubmatch(match)
		alt, url := parts[1], parts[2]
		var html string
		if isSupportedImage(url) {
			imgURLByIdx = append(imgURLByIdx, url)
			html = "" // placeholder text assigned later
		} else {
			imgURLByIdx = append(imgURLByIdx, "") // no media
			html = unsupportedImagePlaceholder(alt)
		}
		placeholder := fmt.Sprintf("\x00IMG%d\x00", len(imagePlaceholders))
		imagePlaceholders = append(imagePlaceholders, html)
		return placeholder
	})

	// Process HTML <img> tags
	s = reHTMLImg.ReplaceAllStringFunc(s, func(match string) string {
		url := reHTMLImg.FindStringSubmatch(match)[1]
		alt := reHTMLImgAlt.FindStringSubmatch(match)
		altText := ""
		if alt != nil {
			altText = alt[1]
		}
		var html string
		if isSupportedImage(url) {
			imgURLByIdx = append(imgURLByIdx, url)
			html = ""
		} else {
			imgURLByIdx = append(imgURLByIdx, "")
			html = unsupportedImagePlaceholder(altText)
		}
		placeholder := fmt.Sprintf("\x00IMG%d\x00", len(imagePlaceholders))
		imagePlaceholders = append(imagePlaceholders, html)
		return placeholder
	})

	// Convert links before escaping (URLs contain & etc.)
	var links []string
	s = reLink.ReplaceAllStringFunc(s, func(match string) string {
		parts := reLink.FindStringSubmatch(match)
		placeholder := fmt.Sprintf("\x00LINK%d\x00", len(links))
		links = append(links, fmt.Sprintf(`<a href="%s">%s</a>`, parts[2], escapeHTML(parts[1])))
		return placeholder
	})

	// Protect blockquotes (after links/images so they contain those placeholders)
	var blockquotes []string
	s = groupBlockquotesWithPlaceholders(s, &blockquotes)

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

	// Restore non-media placeholders
	for i, bq := range blockquotes {
		s = strings.Replace(s, fmt.Sprintf("\x00BLOCKQUOTE%d\x00", i), bq, 1)
	}
	for i, link := range links {
		s = strings.Replace(s, fmt.Sprintf("\x00LINK%d\x00", i), link, 1)
	}
	for i, block := range codeBlocks {
		s = strings.Replace(s, fmt.Sprintf("\x00CODEBLOCK%d\x00", i), block, 1)
	}
	for i, code := range inlineCodes {
		s = strings.Replace(s, fmt.Sprintf("\x00INLINE%d\x00", i), code, 1)
	}

	// Scan for IMG and TABLE placeholders in document order, assign sequential
	// [Image #N] numbers, and build the ordered media list.
	var media []MediaRef
	imgNum := 0
	s = reMediaMarker.ReplaceAllStringFunc(s, func(marker string) string {
		parts := reMediaMarker.FindStringSubmatch(marker)
		kind := parts[1]
		idx := 0
		fmt.Sscanf(parts[2], "%d", &idx)

		switch kind {
		case "IMG":
			// Restore pre-computed HTML for unsupported images
			if idx < len(imagePlaceholders) && imagePlaceholders[idx] != "" {
				return imagePlaceholders[idx]
			}
			imgNum++
			if idx < len(imgURLByIdx) && imgURLByIdx[idx] != "" {
				media = append(media, MediaRef{URL: imgURLByIdx[idx]})
			}
			return fmt.Sprintf("<i>[Image #%d]</i>", imgNum)
		case "TABLE":
			imgNum++
			if idx < len(tableByIdx) {
				t := tableByIdx[idx]
				media = append(media, MediaRef{Table: &t})
			}
			return fmt.Sprintf("<i>[Table #%d]</i>", imgNum)
		}
		return marker
	})

	// Telegram doesn't support <code> inside <a>, strip inner code tags
	s = reCodeInLink.ReplaceAllString(s, "${1}${2}${3}")

	// Collapse runs of 3+ newlines into 2 (single blank line)
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}

	return strings.TrimSpace(s), media
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

// isSupportedImage returns true if the URL points to an image Telegram can display as a photo.
// Badge services and SVGs are not supported.
func isSupportedImage(url string) bool {
	lower := strings.ToLower(url)
	if strings.HasSuffix(lower, ".svg") {
		return false
	}
	unsupportedHosts := []string{
		"img.shields.io",
		"shields.io",
		"badgen.net",
		"badge.fury.io",
		"forthebadge.com",
	}
	for _, host := range unsupportedHosts {
		if strings.Contains(lower, host) {
			return false
		}
	}
	return true
}

func unsupportedImagePlaceholder(alt string) string {
	if alt != "" {
		return "<i>[Image: " + escapeHTML(alt) + "]</i>"
	}
	return "<i>[Image]</i>"
}

// extractTablesWithPlaceholders finds markdown tables, parses them into Table
// structs, and replaces them with placeholders in the text.
func extractTablesWithPlaceholders(s string, out *[]tableimg.Table) string {
	lines := strings.Split(s, "\n")
	var result []string
	var tableLines []string

	flush := func() {
		if len(tableLines) == 0 {
			return
		}
		var header []string
		var rows [][]string
		for i, line := range tableLines {
			m := reTableRow.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if reTableSep.MatchString(line) {
				continue
			}
			cells := strings.Split(m[1], "|")
			for j := range cells {
				cells[j] = strings.TrimSpace(cells[j])
			}
			if i == 0 {
				header = cells
			} else {
				rows = append(rows, cells)
			}
		}
		if header == nil {
			result = append(result, tableLines...)
			tableLines = nil
			return
		}

		placeholder := fmt.Sprintf("\x00TABLE%d\x00", len(*out))
		*out = append(*out, tableimg.Table{Header: header, Rows: rows})
		result = append(result, placeholder)
		tableLines = nil
	}

	for _, line := range lines {
		if reTableRow.MatchString(line) {
			tableLines = append(tableLines, line)
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
