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
	CreateReviewReply(ctx context.Context, repo string, number int, commentID int64, body string) (int64, error)
	GetQuoteContext(ctx context.Context, repo string, number int, commentID int64, isReviewComment bool) (author, body string, err error)
}

// Bot wraps the Telegram bot for sending notifications.
type Bot struct {
	bot            *bot.Bot
	chatID         int64
	autolinkClient AutolinkClient
	autolinkRepo   string
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

// SendPhotos sends one or more photos with an HTML caption to the configured chat.
// The caption is truncated to 1024 characters (Telegram's limit) and attached to the first photo.
// Returns the message ID of the first photo.
func (b *Bot) SendPhotos(ctx context.Context, urls []string, caption string) (int, error) {
	if len([]rune(caption)) > 1024 {
		caption = truncateHTML(caption, 1024)
	}
	if len(urls) == 1 {
		msg, err := b.bot.SendPhoto(ctx, &bot.SendPhotoParams{
			ChatID:    b.chatID,
			Photo:     &models.InputFileString{Data: urls[0]},
			Caption:   caption,
			ParseMode: models.ParseModeHTML,
		})
		if err != nil {
			return 0, err
		}
		return msg.ID, nil
	}
	media := make([]models.InputMedia, len(urls))
	for i, u := range urls {
		p := &models.InputMediaPhoto{Media: u}
		if i == 0 {
			p.Caption = caption
			p.ParseMode = models.ParseModeHTML
		}
		media[i] = p
	}
	msgs, err := b.bot.SendMediaGroup(ctx, &bot.SendMediaGroupParams{
		ChatID: b.chatID,
		Media:  media,
	})
	if err != nil {
		return 0, err
	}
	return msgs[0].ID, nil
}

// react adds an emoji reaction to a message.
func (b *Bot) react(ctx context.Context, chatID int64, msgID int, emoji string) {
	b.bot.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID:    chatID,
		MessageID: msgID,
		Reaction: []models.ReactionType{
			{
				Type:              models.ReactionTypeTypeEmoji,
				ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji},
			},
		},
	})
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
		if msg == nil || msg.Text == "" {
			return
		}

		// Handle replies to tracked notifications
		if msg.ReplyToMessage != nil {
			b.handleReply(ctx, msg, db, gh)
			return
		}

		// Handle autolinks in non-reply messages
		if b.autolinkClient != nil && b.autolinkRepo != "" {
			b.handleAutolinks(ctx, msg)
		}
	})

	mux.Handle("POST "+path, b.bot.WebhookHandler())
}

func (b *Bot) handleReply(ctx context.Context, msg *models.Message, db *store.Store, gh GitHubClient) {
	repo, issueNumber, _, commentID, quoteText, isReviewComment, err := db.Lookup(msg.ReplyToMessage.ID)
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
		_, body, err = gh.GetQuoteContext(ctx, repo, issueNumber, commentID, isReviewComment)
		if err != nil {
			log.Printf("failed to fetch quote context: %v", err)
		}
	}

	var commentBody string
	if body != "" {
		body = stripQuotes(body)
		quoted := quoteLines(body)
		commentBody = fmt.Sprintf("%s\n\n*%s on Telegram:*\n%s", quoted, displayName, replyText)
	} else {
		commentBody = fmt.Sprintf("*%s on Telegram:*\n%s", displayName, replyText)
	}

	var newCommentID int64
	if isReviewComment && commentID > 0 {
		newCommentID, err = gh.CreateReviewReply(ctx, repo, issueNumber, commentID, commentBody)
	} else {
		newCommentID, err = gh.CreateComment(ctx, repo, issueNumber, commentBody)
	}
	if err != nil {
		log.Printf("failed to post comment to %s#%d: %v", repo, issueNumber, err)
		return
	}

	b.react(ctx, msg.Chat.ID, msg.ID, "👀")

	// Save the user's message so replies to it resolve to the new comment
	if err := db.Save(msg.ID, repo, issueNumber, false, newCommentID, "", isReviewComment); err != nil {
		log.Printf("failed to save reply mapping: %v", err)
	}

	log.Printf("posted reply from %s to %s#%d", displayName, repo, issueNumber)
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

// truncateHTML truncates an HTML string to maxRunes runes, ensuring no HTML
// tags are left unclosed. It finds the last safe cut point before maxRunes
// and closes any open tags.
func truncateHTML(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}

	// Leave room for "..." and closing tags
	cut := maxRunes - 20
	if cut < 0 {
		cut = 0
	}
	truncated := string(runes[:cut])

	// Track open tags
	var openTags []string
	i := 0
	for i < len(truncated) {
		if truncated[i] == '<' {
			end := strings.IndexByte(truncated[i:], '>')
			if end == -1 {
				// Incomplete tag at the end — remove it
				truncated = truncated[:i]
				break
			}
			tag := truncated[i+1 : i+end]
			if strings.HasPrefix(tag, "/") {
				// Closing tag — pop from stack
				if len(openTags) > 0 {
					openTags = openTags[:len(openTags)-1]
				}
			} else {
				// Opening tag — extract tag name and push
				name := tag
				if sp := strings.IndexAny(name, " \t\n"); sp != -1 {
					name = name[:sp]
				}
				openTags = append(openTags, name)
			}
			i += end + 1
		} else {
			i++
		}
	}

	// Close open tags in reverse order
	result := truncated + "..."
	for j := len(openTags) - 1; j >= 0; j-- {
		result += "</" + openTags[j] + ">"
	}
	return result
}
