package telegram

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-telegram/bot/models"
)

// AutolinkClient resolves GitHub references for autolink previews.
type AutolinkClient interface {
	GetIssueOrPR(ctx context.Context, repo string, number int) (title, htmlURL, headSHA string, isPR bool, err error)
	GetCommitInfo(ctx context.Context, repo string, sha string) (shortSHA, message, htmlURL string, err error)
	FormatBuildStatus(ctx context.Context, repo string, number int) (string, error)
}

var (
	autolinkIssueRe  = regexp.MustCompile(`(?:^|[^&\w])#(\d+)\b`)
	autolinkCommitRe = regexp.MustCompile(`\b([0-9a-f]{7,40})\b`)
)

// RegisterAutolinkHandler enables autolink previews for GitHub references.
// Messages containing #123 or commit SHAs will trigger a lookup against the given repo.
func (b *Bot) RegisterAutolinkHandler(gh AutolinkClient, defaultRepo string) {
	b.autolinkClient = gh
	b.autolinkRepo = defaultRepo
}

func (b *Bot) handleAutolinks(ctx context.Context, msg *models.Message) {
	if msg.Chat.ID != b.chatID {
		return
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	var parts []string

	if matches := autolinkIssueRe.FindAllStringSubmatch(text, -1); matches != nil {
		for _, match := range matches {
			number, _ := strconv.Atoi(match[1])
			if html := b.formatIssueAutolink(ctx, number); html != "" {
				parts = append(parts, html)
			}
		}
	}

	if matches := autolinkCommitRe.FindAllStringSubmatch(text, -1); matches != nil {
		for _, match := range matches {
			if html := b.formatCommitAutolink(ctx, match[1]); html != "" {
				parts = append(parts, html)
			}
		}
	}

	if len(parts) == 0 {
		return
	}

	msgID, err := b.Send(ctx, strings.Join(parts, "\n\n"))
	if err != nil {
		log.Printf("failed to send autolink notification: %v", err)
		return
	}
	log.Printf("sent autolink notification (msg %d)", msgID)
}

func (b *Bot) formatIssueAutolink(ctx context.Context, number int) string {
	title, htmlURL, _, isPR, err := b.autolinkClient.GetIssueOrPR(ctx, b.autolinkRepo, number)
	if err != nil {
		log.Printf("failed to look up #%d: %v", number, err)
		return ""
	}

	ref := fmt.Sprintf("#%d", number)
	html := fmt.Sprintf(`<a href="%s">%s</a> %s`, htmlURL, ref, escapeAutolink(title))

	if isPR {
		if buildLine, err := b.autolinkClient.FormatBuildStatus(ctx, b.autolinkRepo, number); err == nil && buildLine != "" {
			html += "\n" + buildLine
		}
	}

	return html
}

func (b *Bot) formatCommitAutolink(ctx context.Context, sha string) string {
	shortSHA, message, htmlURL, err := b.autolinkClient.GetCommitInfo(ctx, b.autolinkRepo, sha)
	if err != nil {
		log.Printf("failed to look up commit %s: %v", sha, err)
		return ""
	}

	return fmt.Sprintf(`<a href="%s">%s</a> %s`, htmlURL, shortSHA, escapeAutolink(message))
}

func escapeAutolink(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
