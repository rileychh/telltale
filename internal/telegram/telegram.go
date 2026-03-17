package telegram

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rileychh/telltale/internal/store"
)

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
func (b *Bot) RegisterReplyHandler(mux *http.ServeMux, path string, db *store.Store) {
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "", bot.MatchTypePrefix, func(ctx context.Context, _ *bot.Bot, update *models.Update) {
		msg := update.Message
		if msg == nil || msg.ReplyToMessage == nil {
			return
		}

		repo, issueNumber, _, err := db.Lookup(msg.ReplyToMessage.ID)
		if err != nil {
			log.Printf("reply lookup failed: %v", err)
			return
		}

		log.Printf("reply from @%s to %s#%d: %s", msg.From.Username, repo, issueNumber, msg.Text)
		// TODO: post comment to GitHub via API
	})

	mux.Handle("POST "+path, b.bot.WebhookHandler())
}
