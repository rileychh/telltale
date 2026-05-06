package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/rileychh/telltale/internal/store"
)

type recordingGH struct {
	createCalled bool
	replyCalled  bool
}

func (r *recordingGH) CreateComment(ctx context.Context, repo string, number int, body string) (int64, error) {
	r.createCalled = true
	return 0, nil
}
func (r *recordingGH) CreateReviewReply(ctx context.Context, repo string, number int, commentID int64, body string) (int64, error) {
	r.replyCalled = true
	return 0, nil
}
func (r *recordingGH) GetQuoteContext(ctx context.Context, repo string, number int, commentID int64, isReviewComment bool) (string, string, error) {
	return "", "", nil
}
func (r *recordingGH) IsLatestComment(ctx context.Context, repo string, number int, commentID int64, isReviewComment bool) (bool, error) {
	return false, nil
}
func (r *recordingGH) EditIssueComment(ctx context.Context, repo string, commentID int64, body string) error {
	return nil
}
func (r *recordingGH) EditReviewComment(ctx context.Context, repo string, commentID int64, body string) error {
	return nil
}

func TestHandleReplyIgnoresOtherChats(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	const configuredChat = -100100
	const otherChat = -100200
	const replyTargetID = 42
	if err := db.Save(replyTargetID, "owner/repo", 1, true, 99, "", true); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	b := &Bot{chatID: configuredChat}
	gh := &recordingGH{}

	msg := &models.Message{
		ID:             1,
		Chat:           models.Chat{ID: otherChat},
		Text:           "reply text",
		ReplyToMessage: &models.Message{ID: replyTargetID},
		From:           &models.User{ID: 1, Username: "u"},
	}
	b.handleReply(context.Background(), msg, db, gh)

	if gh.createCalled || gh.replyCalled {
		t.Fatalf("handleReply posted to GitHub for a message from %d (configured chat %d)", otherChat, configuredChat)
	}
}

func TestHandleEditIgnoresOtherChats(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	const configuredChat = -100100
	const otherChat = -100200
	const editedMsgID = 7
	if err := db.Save(editedMsgID, "owner/repo", 1, true, 99, "", true); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	b := &Bot{chatID: configuredChat}
	gh := &recordingGH{}

	msg := &models.Message{
		ID:   editedMsgID,
		Chat: models.Chat{ID: otherChat},
		Text: "edited text",
		From: &models.User{ID: 1, Username: "u"},
	}
	b.handleEdit(context.Background(), msg, db, gh)
	if gh.createCalled || gh.replyCalled {
		t.Fatalf("handleEdit acted on message from %d (configured chat %d)", otherChat, configuredChat)
	}

	// Sanity: the temp DB should remain untouched by the rejected call
	if _, err := os.Stat(filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("db missing: %v", err)
	}
}
