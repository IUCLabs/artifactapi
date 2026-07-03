package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// GetSigningKey returns the stored armored private key and key id for a purpose.
// found is false when no key has been generated yet.
func (db *DB) GetSigningKey(ctx context.Context, purpose string) (armor, keyID string, found bool, err error) {
	row := db.Pool.QueryRow(ctx, `
		SELECT private_key_armor, key_id FROM signing_keys WHERE purpose = $1
	`, purpose)
	if err := row.Scan(&armor, &keyID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return armor, keyID, true, nil
}

// InsertSigningKeyIfAbsent stores a freshly generated key, doing nothing if
// another replica already inserted one. Callers re-read with GetSigningKey to
// pick up whichever key won the race.
func (db *DB) InsertSigningKeyIfAbsent(ctx context.Context, purpose, armor, keyID string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO signing_keys (purpose, private_key_armor, key_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (purpose) DO NOTHING
	`, purpose, armor, keyID)
	return err
}
