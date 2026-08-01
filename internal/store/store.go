package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store maps Telegram message IDs to GitHub issue/PR numbers for reply routing.
type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS message_map (
			telegram_msg_id    INTEGER PRIMARY KEY,
			repo               TEXT NOT NULL,
			issue_number       INTEGER NOT NULL,
			is_pr              BOOLEAN NOT NULL DEFAULT FALSE,
			comment_id         INTEGER NOT NULL DEFAULT 0,
			quote_text         TEXT NOT NULL DEFAULT '',
			is_review_comment  BOOLEAN NOT NULL DEFAULT FALSE
		)
	`); err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}

	// Migration: add is_review_comment column if missing
	db.Exec(`ALTER TABLE message_map ADD COLUMN is_review_comment BOOLEAN NOT NULL DEFAULT FALSE`)

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS thread_latest (
			repo          TEXT NOT NULL,
			issue_number  INTEGER NOT NULL,
			latest_msg_id INTEGER NOT NULL,
			PRIMARY KEY (repo, issue_number)
		)
	`); err != nil {
		return nil, fmt.Errorf("create thread_latest table: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS entity_index (
			repo            TEXT NOT NULL,
			entity_type     TEXT NOT NULL,
			entity_id       INTEGER NOT NULL,
			telegram_msg_id INTEGER NOT NULL,
			PRIMARY KEY (repo, entity_type, entity_id)
		)
	`); err != nil {
		return nil, fmt.Errorf("create entity_index table: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_entity_msg ON entity_index(telegram_msg_id)`); err != nil {
		return nil, fmt.Errorf("create entity_msg index: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Save records a mapping from a Telegram message to a GitHub issue/PR.
// commentID is the GitHub comment ID for comment notifications (0 otherwise).
// quoteText overrides the quote context when set (used for reviews).
func (s *Store) Save(telegramMsgID int, repo string, issueNumber int, isPR bool, commentID int64, quoteText string, isReviewComment bool) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO message_map (telegram_msg_id, repo, issue_number, is_pr, comment_id, quote_text, is_review_comment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		telegramMsgID, repo, issueNumber, isPR, commentID, quoteText, isReviewComment,
	)
	return err
}

// SaveLatest records the most recent bot-sent notification message for an issue/PR.
func (s *Store) SaveLatest(repo string, issueNumber int, telegramMsgID int) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO thread_latest (repo, issue_number, latest_msg_id) VALUES (?, ?, ?)`,
		repo, issueNumber, telegramMsgID,
	)
	return err
}

// LookupLatest returns the most recent bot-sent notification message ID for an
// issue/PR, or 0 if none is known.
func (s *Store) LookupLatest(repo string, issueNumber int) (int, error) {
	var msgID int
	err := s.db.QueryRow(
		`SELECT latest_msg_id FROM thread_latest WHERE repo = ? AND issue_number = ?`,
		repo, issueNumber,
	).Scan(&msgID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return msgID, err
}

// Lookup finds the GitHub issue/PR associated with a Telegram message.
func (s *Store) Lookup(telegramMsgID int) (repo string, issueNumber int, isPR bool, commentID int64, quoteText string, isReviewComment bool, err error) {
	err = s.db.QueryRow(
		`SELECT repo, issue_number, is_pr, comment_id, quote_text, is_review_comment FROM message_map WHERE telegram_msg_id = ?`,
		telegramMsgID,
	).Scan(&repo, &issueNumber, &isPR, &commentID, &quoteText, &isReviewComment)
	return
}

// LinkEntity associates a GitHub entity (issue body, PR body, comment, review,
// or inline comment within a consolidated review) with the Telegram message
// that displays it. Multiple entities may map to the same Telegram message
// (e.g. all inline comments in a consolidated review).
func (s *Store) LinkEntity(repo, entityType string, entityID int64, telegramMsgID int) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO entity_index (repo, entity_type, entity_id, telegram_msg_id) VALUES (?, ?, ?, ?)`,
		repo, entityType, entityID, telegramMsgID,
	)
	return err
}

// LookupEntity returns the Telegram message ID for a GitHub entity. Returns
// sql.ErrNoRows when there is no link.
func (s *Store) LookupEntity(repo, entityType string, entityID int64) (telegramMsgID int, err error) {
	err = s.db.QueryRow(
		`SELECT telegram_msg_id FROM entity_index WHERE repo = ? AND entity_type = ? AND entity_id = ?`,
		repo, entityType, entityID,
	).Scan(&telegramMsgID)
	return
}

// UnlinkEntity removes the link for a single GitHub entity. Used after a
// comment is deleted on GitHub.
func (s *Store) UnlinkEntity(repo, entityType string, entityID int64) error {
	_, err := s.db.Exec(
		`DELETE FROM entity_index WHERE repo = ? AND entity_type = ? AND entity_id = ?`,
		repo, entityType, entityID,
	)
	return err
}

// UnlinkAllForMessage removes all entity links pointing to a Telegram message.
// Used when a message is deleted (e.g. cascading cleanup).
func (s *Store) UnlinkAllForMessage(telegramMsgID int) error {
	_, err := s.db.Exec(
		`DELETE FROM entity_index WHERE telegram_msg_id = ?`,
		telegramMsgID,
	)
	return err
}
