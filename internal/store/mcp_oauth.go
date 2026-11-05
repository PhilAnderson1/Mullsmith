package store

import (
	"context"
	"time"
)

type MCPOAuthConnection struct {
	UserID, ServerID string
	TokenCipher      []byte
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (d *DB) PutMCPOAuthConnection(ctx context.Context, c *MCPOAuthConnection) error {
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO mcp_oauth_connections(user_id,server_id,token_cipher,created_at,updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(user_id,server_id) DO UPDATE SET token_cipher=excluded.token_cipher, updated_at=excluded.updated_at`,
		c.UserID, c.ServerID, c.TokenCipher, c.CreatedAt.Unix(), c.UpdatedAt.Unix())
	return err
}

func (d *DB) MCPOAuthConnection(ctx context.Context, userID, serverID string) (*MCPOAuthConnection, error) {
	var c MCPOAuthConnection
	var created, updated int64
	err := d.sqlDB.QueryRowContext(ctx, `SELECT user_id,server_id,token_cipher,created_at,updated_at
		FROM mcp_oauth_connections WHERE user_id=? AND server_id=?`, userID, serverID).
		Scan(&c.UserID, &c.ServerID, &c.TokenCipher, &created, &updated)
	if err == ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &c, nil
}

func (d *DB) MCPOAuthConnections(ctx context.Context) ([]MCPOAuthConnection, error) {
	rows, err := d.sqlDB.QueryContext(ctx, `SELECT user_id,server_id,token_cipher,created_at,updated_at FROM mcp_oauth_connections`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPOAuthConnection
	for rows.Next() {
		var c MCPOAuthConnection
		var created, updated int64
		if err := rows.Scan(&c.UserID, &c.ServerID, &c.TokenCipher, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt, c.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) DeleteMCPOAuthConnection(ctx context.Context, userID, serverID string) error {
	_, err := d.sqlDB.ExecContext(ctx, `DELETE FROM mcp_oauth_connections WHERE user_id=? AND server_id=?`, userID, serverID)
	return err
}
