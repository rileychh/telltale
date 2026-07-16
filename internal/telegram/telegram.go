package telegram

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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
	IsLatestComment(ctx context.Context, repo string, number int, commentID int64, isReviewComment bool) (bool, error)
	EditIssueComment(ctx context.Context, repo string, commentID int64, body string) error
	EditReviewComment(ctx context.Context, repo string, commentID int64, body string) error
}

// Bot wraps the Telegram bot for sending notifications.
type Bot struct {
	bot            *bot.Bot
	chatID         int64
	allowedRepos   map[string]bool
	autolinkClient AutolinkClient
	autolinkRepo   string
}

func New(token string, chatID string, allowedRepos []string) (*Bot, error) {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid chat ID %q: %w", chatID, err)
	}

	b, err := bot.New(token, bot.WithSkipGetMe())
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}

	var allow map[string]bool
	if len(allowedRepos) > 0 {
		allow = make(map[string]bool, len(allowedRepos))
		for _, r := range allowedRepos {
			allow[r] = true
		}
	}

	return &Bot{bot: b, chatID: id, allowedRepos: allow}, nil
}

// repoAllowed reports whether the bot is configured to route replies to repo.
// An empty allowlist permits everything (preserves prior behavior).
func (b *Bot) repoAllowed(repo string) bool {
	if b.allowedRepos == nil {
		return true
	}
	return b.allowedRepos[repo]
}

// Send sends an HTML-formatted message to the configured chat and returns the message ID.
// If replyTo > 0, the message is sent as a reply to that message.
func (b *Bot) Send(ctx context.Context, html string, replyTo int) (int, error) {
	if len([]rune(html)) > 4096 {
		html = truncateHTML(html, 4096)
	}
	msg, err := b.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    b.chatID,
		Text:      html,
		ParseMode: models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{
			IsDisabled: bot.True(),
		},
		ReplyParameters: replyParams(replyTo),
	})
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

// replyParams builds a ReplyParameters value for the given message ID, or nil
// if no reply target is set. AllowSendingWithoutReply ensures the send still
// succeeds if the referenced message was deleted.
func replyParams(replyTo int) *models.ReplyParameters {
	if replyTo <= 0 {
		return nil
	}
	return &models.ReplyParameters{
		MessageID:                replyTo,
		AllowSendingWithoutReply: true,
	}
}

// MediaItem represents a photo to send, either by URL or as raw bytes.
type MediaItem struct {
	URL  string // URL-based photo (mutually exclusive with Data)
	Data []byte // Generated image bytes (mutually exclusive with URL)
	Name string // Filename for uploaded images
}

// SendMedia sends one or more photos (URL or uploaded bytes) with an HTML caption.
// If replyTo > 0, the message is sent as a reply to that message.
func (b *Bot) SendMedia(ctx context.Context, items []MediaItem, caption string, replyTo int) (int, error) {
	if len([]rune(caption)) > 1024 {
		caption = truncateHTML(caption, 1024)
	}
	if len(items) == 1 {
		item := items[0]
		var photo models.InputFile
		if item.URL != "" {
			photo = &models.InputFileString{Data: item.URL}
		} else {
			photo = &models.InputFileUpload{Filename: item.Name, Data: bytes.NewReader(item.Data)}
		}
		msg, err := b.bot.SendPhoto(ctx, &bot.SendPhotoParams{
			ChatID:          b.chatID,
			Photo:           photo,
			Caption:         caption,
			ParseMode:       models.ParseModeHTML,
			ReplyParameters: replyParams(replyTo),
		})
		if err != nil {
			return 0, err
		}
		return msg.ID, nil
	}
	media := make([]models.InputMedia, len(items))
	for i, item := range items {
		p := &models.InputMediaPhoto{}
		if item.URL != "" {
			p.Media = item.URL
		} else {
			p.Media = "attach://" + item.Name
			p.MediaAttachment = bytes.NewReader(item.Data)
		}
		if i == 0 {
			p.Caption = caption
			p.ParseMode = models.ParseModeHTML
		}
		media[i] = p
	}
	msgs, err := b.bot.SendMediaGroup(ctx, &bot.SendMediaGroupParams{
		ChatID:          b.chatID,
		Media:           media,
		ReplyParameters: replyParams(replyTo),
	})
	if err != nil {
		return 0, err
	}
	return msgs[0].ID, nil
}

// React adds an emoji reaction to a message in the configured chat.
func (b *Bot) React(ctx context.Context, msgID int, emoji string) {
	b.react(ctx, b.chatID, msgID, emoji)
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

// EditMessage edits the text of a previously sent text message.
func (b *Bot) EditMessage(ctx context.Context, msgID int, html string) error {
	if len([]rune(html)) > 4096 {
		html = truncateHTML(html, 4096)
	}
	_, err := b.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    b.chatID,
		MessageID: msgID,
		Text:      html,
		ParseMode: models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{
			IsDisabled: bot.True(),
		},
	})
	if isNotModified(err) {
		return nil
	}
	return err
}

// EditCaption edits the caption of a previously sent photo or media group.
// The caption is truncated to Telegram's 1024-rune limit.
func (b *Bot) EditCaption(ctx context.Context, msgID int, caption string) error {
	if len([]rune(caption)) > 1024 {
		caption = truncateHTML(caption, 1024)
	}
	_, err := b.bot.EditMessageCaption(ctx, &bot.EditMessageCaptionParams{
		ChatID:    b.chatID,
		MessageID: msgID,
		Caption:   caption,
		ParseMode: models.ParseModeHTML,
	})
	if isNotModified(err) {
		return nil
	}
	return err
}

// EditOrCaption edits a text message's text or a media message's caption,
// dispatching based on hasMedia recorded at send time.
func (b *Bot) EditOrCaption(ctx context.Context, msgID int, hasMedia bool, content string) error {
	if hasMedia {
		return b.EditCaption(ctx, msgID, content)
	}
	return b.EditMessage(ctx, msgID, content)
}

// DeleteMessage deletes a message from the configured chat. A
// "message to delete not found" response is treated as success.
func (b *Bot) DeleteMessage(ctx context.Context, msgID int) error {
	_, err := b.bot.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID:    b.chatID,
		MessageID: msgID,
	})
	if isAlreadyGone(err) {
		return nil
	}
	return err
}

// isNotModified reports whether err is Telegram's "message is not modified"
// response, which we treat as a successful no-op.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// isAlreadyGone reports whether err indicates the target message no longer
// exists, which makes a delete request trivially successful.
func isAlreadyGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "message to delete not found") ||
		strings.Contains(msg, "message can't be deleted")
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

		// Handle replies to tracked notifications. A reply to an untracked
		// message (e.g. an ordinary chat message) is not consumed here, so it
		// falls through to autolink handling below.
		if msg.ReplyToMessage != nil && b.handleReply(ctx, msg, db, gh) {
			return
		}

		// Handle autolinks in non-reply messages and in replies to untracked ones.
		if b.autolinkClient != nil && b.autolinkRepo != "" {
			b.handleAutolinks(ctx, msg)
		}
	})

	// Handle autolinks in media captions (photos, videos, documents, etc.)
	b.bot.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.Message != nil && update.Message.Caption != ""
	}, func(ctx context.Context, _ *bot.Bot, update *models.Update) {
		if b.autolinkClient != nil && b.autolinkRepo != "" {
			b.handleAutolinks(ctx, update.Message)
		}
	})

	// Propagate user-side edits of a tracked reply to the GitHub comment we
	// created from that reply.
	b.bot.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.EditedMessage != nil && update.EditedMessage.Text != ""
	}, func(ctx context.Context, _ *bot.Bot, update *models.Update) {
		b.handleEdit(ctx, update.EditedMessage, db, gh)
	})

	mux.Handle("POST "+path, b.bot.WebhookHandler())
}

// handleReply posts a Telegram reply as a GitHub comment when the replied-to
// message maps to a tracked notification. It reports whether the reply was
// consumed: false means the reply target is untracked, so the caller should try
// autolink handling instead.
func (b *Bot) handleReply(ctx context.Context, msg *models.Message, db *store.Store, gh GitHubClient) bool {
	if msg.Chat.ID != b.chatID {
		return false
	}
	repo, issueNumber, _, commentID, quoteText, isReviewComment, err := db.Lookup(msg.ReplyToMessage.ID)
	if err != nil {
		// No mapping means this is a reply to an ordinary chat message, not a
		// tracked notification — let autolink handling take over.
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("reply lookup failed: %v", err)
		}
		return false
	}
	if !b.repoAllowed(repo) {
		log.Printf("dropped reply: mapping to disallowed repo %s#%d", repo, issueNumber)
		return true
	}

	displayName := telegramDisplayName(msg.From)
	commentBody := buildCommentBody(ctx, gh, msg, displayName, repo, issueNumber, commentID, quoteText, isReviewComment)

	var newCommentID int64
	if isReviewComment && commentID > 0 {
		newCommentID, err = gh.CreateReviewReply(ctx, repo, issueNumber, commentID, commentBody)
	} else {
		newCommentID, err = gh.CreateComment(ctx, repo, issueNumber, commentBody)
	}
	if err != nil {
		log.Printf("failed to post comment to %s#%d: %v", repo, issueNumber, err)
		return true
	}

	b.react(ctx, msg.Chat.ID, msg.ID, "👀")

	// Save the user's message so replies to it resolve to the new comment
	if err := db.Save(msg.ID, repo, issueNumber, false, newCommentID, "", isReviewComment); err != nil {
		log.Printf("failed to save reply mapping: %v", err)
	}

	// Extend the reply chain: subsequent GitHub events for this issue/PR should
	// reply to the user's Telegram message rather than skipping over it.
	if err := db.SaveLatest(repo, issueNumber, msg.ID); err != nil {
		log.Printf("failed to update latest message: %v", err)
	}

	log.Printf("posted reply from %s to %s#%d", displayName, repo, issueNumber)
	return true
}

// handleEdit propagates an edit of a tracked Telegram reply to the GitHub
// comment that was originally created from it.
func (b *Bot) handleEdit(ctx context.Context, msg *models.Message, db *store.Store, gh GitHubClient) {
	if msg.Chat.ID != b.chatID {
		return
	}
	// Defense in depth: a bot can never edit a user's reply, and Telegram does
	// not normally fire edited_message for the bot's own message edits, but
	// guard against it so a stray update never PATCHes someone else's GitHub
	// comment using the bot's notification mapping.
	if msg.From == nil || msg.From.IsBot {
		return
	}

	repo, issueNumber, _, newCommentID, _, isReviewComment, err := db.Lookup(msg.ID)
	if err != nil || newCommentID == 0 {
		return
	}
	if !b.repoAllowed(repo) {
		log.Printf("dropped edit: mapping to disallowed repo %s#%d", repo, issueNumber)
		return
	}

	// Re-derive the quote target from the original reply chain.
	var targetCommentID int64
	var targetQuoteText string
	var targetIsReviewComment bool
	if msg.ReplyToMessage != nil {
		_, _, _, targetCommentID, targetQuoteText, targetIsReviewComment, err = db.Lookup(msg.ReplyToMessage.ID)
		if err != nil {
			log.Printf("edit: target lookup failed: %v", err)
		}
	}

	displayName := telegramDisplayName(msg.From)
	commentBody := buildCommentBody(ctx, gh, msg, displayName, repo, issueNumber, targetCommentID, targetQuoteText, targetIsReviewComment)

	if isReviewComment {
		err = gh.EditReviewComment(ctx, repo, newCommentID, commentBody)
	} else {
		err = gh.EditIssueComment(ctx, repo, newCommentID, commentBody)
	}
	if err != nil {
		log.Printf("failed to edit comment %d on %s: %v", newCommentID, repo, err)
		return
	}

	b.react(ctx, msg.Chat.ID, msg.ID, "👀")
	log.Printf("edited GitHub comment %d on %s#%d from %s", newCommentID, repo, issueNumber, displayName)
}

// buildCommentBody constructs the GitHub comment body for a Telegram reply.
// It quotes the target context (manual selection, cached quoteText, or fetched
// from GitHub) above a "*Name on Telegram:*" header followed by the reply
// text. Used by both the create and edit paths so they stay in lockstep.
func buildCommentBody(ctx context.Context, gh GitHubClient, msg *models.Message, displayName, repo string, issueNumber int, commentID int64, quoteText string, isReviewComment bool) string {
	replyText := entitiesToMarkdown(msg.Text, msg.Entities)

	var body string
	var manualQuote bool
	if msg.Quote != nil && msg.Quote.IsManual && msg.Quote.Text != "" {
		body = entitiesToMarkdown(msg.Quote.Text, msg.Quote.Entities)
		manualQuote = true
	} else if quoteText != "" {
		body = quoteText
	} else if commentID > 0 {
		var err error
		_, body, err = gh.GetQuoteContext(ctx, repo, issueNumber, commentID, isReviewComment)
		if err != nil {
			log.Printf("failed to fetch quote context: %v", err)
		}
	}

	// Suppress an auto-derived quote when GitHub will already render the
	// referenced comment directly above this reply.
	if body != "" && !manualQuote && commentID > 0 {
		latest, err := gh.IsLatestComment(ctx, repo, issueNumber, commentID, isReviewComment)
		if err != nil {
			log.Printf("failed to check latest comment: %v", err)
		} else if latest {
			body = ""
		}
	}

	if body == "" {
		return fmt.Sprintf("*%s on Telegram:*\n%s", displayName, replyText)
	}
	if !manualQuote {
		body = stripQuotes(body)
	}
	return fmt.Sprintf("%s\n\n*%s on Telegram:*\n%s", quoteLines(body), displayName, replyText)
}

// telegramDisplayName returns a human-readable name for a Telegram user,
// preferring @username, then full name, and falling back to numeric ID.
func telegramDisplayName(u *models.User) string {
	if u == nil {
		return "[unknown]"
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		return name
	}
	return fmt.Sprintf("[%d]", u.ID)
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

	// If the cut split an HTML entity (e.g. `&am` instead of `&amp;`), drop it.
	// Safe to run after the tag loop: any incomplete trailing tag has already
	// been stripped, so the only `&` without a following `;` is in plain text.
	if amp := strings.LastIndexByte(truncated, '&'); amp != -1 {
		if !strings.ContainsRune(truncated[amp:], ';') {
			truncated = truncated[:amp]
		}
	}

	// Close open tags in reverse order
	result := truncated + "..."
	for j := len(openTags) - 1; j >= 0; j-- {
		result += "</" + openTags[j] + ">"
	}
	return result
}
