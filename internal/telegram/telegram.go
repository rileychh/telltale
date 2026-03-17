package telegram

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rileychh/telltale/internal/store"
)

// GitHubClient posts comments and fetches context from GitHub.
type GitHubClient interface {
	CreateComment(ctx context.Context, repo string, number int, body string) (int64, error)
	GetQuoteContext(ctx context.Context, repo string, number int, commentID int64) (author, body string, err error)
}

// Bot wraps the Telegram bot for sending notifications.
type Bot struct {
	bot    *bot.Bot
	chatID int64
}

func New(token string, chatID string) (*Bot, error) {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid chat ID %q: %w", chatID, err)
	}

	b, err := bot.New(token, bot.WithSkipGetMe())
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}

	return &Bot{bot: b, chatID: id}, nil
}

// Send sends an HTML-formatted message to the configured chat and returns the message ID.
func (b *Bot) Send(ctx context.Context, html string) (int, error) {
	msg, err := b.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    b.chatID,
		Text:      html,
		ParseMode: models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{
			IsDisabled: bot.True(),
		},
	})
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

// StartWebhook starts processing incoming Telegram updates.
func (b *Bot) StartWebhook(ctx context.Context) {
	b.bot.StartWebhook(ctx)
}

// RegisterReplyHandler sets up the webhook handler for incoming Telegram updates.
// When a user replies to a notification, the reply is posted as a GitHub comment.
func (b *Bot) RegisterReplyHandler(mux *http.ServeMux, path string, db *store.Store, gh GitHubClient) {
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "", bot.MatchTypePrefix, func(ctx context.Context, _ *bot.Bot, update *models.Update) {
		msg := update.Message
		if msg == nil || msg.ReplyToMessage == nil {
			return
		}

		repo, issueNumber, _, commentID, quoteText, err := db.Lookup(msg.ReplyToMessage.ID)
		if err != nil {
			log.Printf("reply lookup failed: %v", err)
			return
		}

		displayName := strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)
		if displayName == "" {
			displayName = msg.From.Username
		}

		replyText := entitiesToMarkdown(msg.Text, msg.Entities)

		// Fetch the original content to quote
		var body string
		if quoteText != "" {
			body = quoteText
		} else {
			_, body, err = gh.GetQuoteContext(ctx, repo, issueNumber, commentID)
			if err != nil {
				log.Printf("failed to fetch quote context: %v", err)
			}
		}

		var comment string
		if body != "" {
			body = stripQuotes(body)
			quoted := quoteLines(body)
			comment = fmt.Sprintf("%s\n\n*%s on Telegram:*\n%s", quoted, displayName, replyText)
		} else {
			comment = fmt.Sprintf("*%s on Telegram:*\n%s", displayName, replyText)
		}

		newCommentID, err := gh.CreateComment(ctx, repo, issueNumber, comment)
		if err != nil {
			log.Printf("failed to post comment to %s#%d: %v", repo, issueNumber, err)
			return
		}

		// Save the user's message so replies to it resolve to the new comment
		if err := db.Save(msg.ID, repo, issueNumber, false, newCommentID, ""); err != nil {
			log.Printf("failed to save reply mapping: %v", err)
		}

		log.Printf("posted reply from %s to %s#%d", displayName, repo, issueNumber)
	})

	mux.Handle("POST "+path, b.bot.WebhookHandler())
}

// entitiesToMarkdown converts Telegram text + entities to GitHub-flavored Markdown.
func entitiesToMarkdown(text string, entities []models.MessageEntity) string {
	if len(entities) == 0 {
		return text
	}

	runes := []rune(text)
	// Build open/close tags indexed by rune position
	opens := make(map[int][]string)
	closes := make(map[int][]string)

	for _, e := range entities {
		start := e.Offset
		end := e.Offset + e.Length

		var open, close string
		switch e.Type {
		case "bold":
			open, close = "**", "**"
		case "italic":
			open, close = "_", "_"
		case "underline":
			open, close = "<u>", "</u>"
		case "strikethrough":
			open, close = "~~", "~~"
		case "code":
			open, close = "`", "`"
		case "pre":
			lang := ""
			if e.Language != "" {
				lang = e.Language
			}
			open, close = "```"+lang, "\n```"
		case "text_link":
			open = "["
			close = fmt.Sprintf("](%s)", e.URL)
		case "url":
			continue
		default:
			continue
		}

		opens[start] = append(opens[start], open)
		closes[end] = append([]string{close}, closes[end]...)
	}

	var b strings.Builder
	for i, r := range runes {
		for _, tag := range closes[i] {
			b.WriteString(tag)
		}
		for _, tag := range opens[i] {
			b.WriteString(tag)
		}
		b.WriteRune(r)
	}
	// Close any remaining tags at the end
	for _, tag := range closes[len(runes)] {
		b.WriteString(tag)
	}

	return b.String()
}

// stripQuotes removes blockquote lines and the "*Name on Telegram:*" header,
// leaving only the actual message content.
func stripQuotes(s string) string {
	lines := strings.Split(s, "\n")
	var result []string
	skipNext := false
	for _, line := range lines {
		if strings.HasPrefix(line, ">") {
			continue
		}
		if strings.HasPrefix(line, "*") && strings.Contains(line, "on Telegram:*") {
			skipNext = true
			continue
		}
		if skipNext && line == "" {
			skipNext = false
			continue
		}
		result = append(result, line)
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

// quoteLines prefixes each line with "> " for GitHub Markdown quoting.
func quoteLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}
