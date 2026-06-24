package push

import (
	"context"
	"database/sql"
	"fmt"
)

// Token is one registered push token for a workspace.
type Token struct {
	ID          string
	WorkspaceID string
	Token       string
	Platform    string
	CreatedAt   string
}

// Repo reads and writes push tokens in Postgres.
type Repo struct {
	db *sql.DB
}

// NewRepo creates a token repository backed by db.
func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// SaveToken registers a push token for a workspace. If the same token already
// exists for the workspace, it updates the timestamp.
func (r *Repo) SaveToken(ctx context.Context, workspaceID, token, platform string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO push_tokens (workspace_id, token, platform)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, token) DO UPDATE
		SET updated_at = now()
	`, workspaceID, token, platform)
	if err != nil {
		return fmt.Errorf("push_tokens: save: %w", err)
	}
	return nil
}

// DeleteToken removes a push token. Returns nil even if the token did not exist.
func (r *Repo) DeleteToken(ctx context.Context, workspaceID, token string) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM push_tokens
		WHERE workspace_id = $1 AND token = $2
	`, workspaceID, token)
	if err != nil {
		return fmt.Errorf("push_tokens: delete: %w", err)
	}
	return nil
}

// GetTokens returns all active push tokens for a workspace.
func (r *Repo) GetTokens(ctx context.Context, workspaceID string) ([]Token, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, workspace_id, token, platform, created_at
		FROM push_tokens
		WHERE workspace_id = $1
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("push_tokens: list: %w", err)
	}
	defer rows.Close()

	var tokens []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.Token, &t.Platform, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("push_tokens: scan: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}
