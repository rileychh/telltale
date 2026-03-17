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
			telegram_msg_id INTEGER PRIMARY KEY,
			repo            TEXT NOT NULL,
			issue_number    INTEGER NOT NULL,
			is_pr           BOOLEAN NOT NULL DEFAULT FALSE,
			comment_id      INTEGER NOT NULL DEFAULT 0,
			quote_text      TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Save records a mapping from a Telegram message to a GitHub issue/PR.
// commentID is the GitHub comment ID for comment notifications (0 otherwise).
// quoteText overrides the quote context when set (used for reviews).
func (s *Store) Save(telegramMsgID int, repo string, issueNumber int, isPR bool, commentID int64, quoteText string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO message_map (telegram_msg_id, repo, issue_number, is_pr, comment_id, quote_text) VALUES (?, ?, ?, ?, ?, ?)`,
		telegramMsgID, repo, issueNumber, isPR, commentID, quoteText,
	)
	return err
}

// Lookup finds the GitHub issue/PR associated with a Telegram message.
func (s *Store) Lookup(telegramMsgID int) (repo string, issueNumber int, isPR bool, commentID int64, quoteText string, err error) {
	err = s.db.QueryRow(
		`SELECT repo, issue_number, is_pr, comment_id, quote_text FROM message_map WHERE telegram_msg_id = ?`,
		telegramMsgID,
	).Scan(&repo, &issueNumber, &isPR, &commentID, &quoteText)
	return
}
