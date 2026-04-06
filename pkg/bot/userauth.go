package bot

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// UserAuth stores per-Matrix-user WordPress agent tokens in SQLite.
type UserAuth struct {
	db *sql.DB
	mu sync.RWMutex
}

// NewUserAuth opens (or creates) the SQLite database and ensures the schema exists.
func NewUserAuth(dbPath string) (*UserAuth, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open user auth database: %w", err)
	}

	schema := `CREATE TABLE IF NOT EXISTS user_tokens (
		matrix_user_id TEXT PRIMARY KEY,
		agent_token    TEXT NOT NULL,
		site_url       TEXT NOT NULL,
		created_at     DATETIME DEFAULT CURRENT_TIMESTAMP
	)`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create user_tokens table: %w", err)
	}

	return &UserAuth{db: db}, nil
}

// GetToken returns the stored agent token for a Matrix user, or empty string if none.
func (ua *UserAuth) GetToken(matrixUserID string) (string, error) {
	ua.mu.RLock()
	defer ua.mu.RUnlock()

	var token string
	err := ua.db.QueryRow(
		"SELECT agent_token FROM user_tokens WHERE matrix_user_id = ?",
		matrixUserID,
	).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get token for %s: %w", matrixUserID, err)
	}
	return token, nil
}

// SaveToken stores (or replaces) the agent token for a Matrix user.
func (ua *UserAuth) SaveToken(matrixUserID, agentToken, siteURL string) error {
	ua.mu.Lock()
	defer ua.mu.Unlock()

	_, err := ua.db.Exec(
		`INSERT INTO user_tokens (matrix_user_id, agent_token, site_url, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(matrix_user_id) DO UPDATE SET
		   agent_token = excluded.agent_token,
		   site_url    = excluded.site_url,
		   created_at  = excluded.created_at`,
		matrixUserID, agentToken, siteURL, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("failed to save token for %s: %w", matrixUserID, err)
	}
	return nil
}

// DeleteToken removes the stored token for a Matrix user.
func (ua *UserAuth) DeleteToken(matrixUserID string) error {
	ua.mu.Lock()
	defer ua.mu.Unlock()

	_, err := ua.db.Exec(
		"DELETE FROM user_tokens WHERE matrix_user_id = ?",
		matrixUserID,
	)
	if err != nil {
		return fmt.Errorf("failed to delete token for %s: %w", matrixUserID, err)
	}
	return nil
}

// Close closes the underlying database connection.
func (ua *UserAuth) Close() error {
	return ua.db.Close()
}
