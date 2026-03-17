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
			is_pr           BOOLEAN NOT NULL DEFAULT FALSE
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
func (s *Store) Save(telegramMsgID int, repo string, issueNumber int, isPR bool) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO message_map (telegram_msg_id, repo, issue_number, is_pr) VALUES (?, ?, ?, ?)`,
		telegramMsgID, repo, issueNumber, isPR,
	)
	return err
}

// Lookup finds the GitHub issue/PR associated with a Telegram message.
func (s *Store) Lookup(telegramMsgID int) (repo string, issueNumber int, isPR bool, err error) {
	err = s.db.QueryRow(
		`SELECT repo, issue_number, is_pr FROM message_map WHERE telegram_msg_id = ?`,
		telegramMsgID,
	).Scan(&repo, &issueNumber, &isPR)
	return
}
