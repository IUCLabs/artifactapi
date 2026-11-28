package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// ErrGitHubAppNotFound is returned when no GitHub App has the requested name.
var ErrGitHubAppNotFound = errors.New("github app not found")

// GitHubAppInUseError is returned when deleting a GitHub App still referenced by remotes.
type GitHubAppInUseError struct {
	Name    string
	Remotes []string
}

func (e *GitHubAppInUseError) Error() string {
	return fmt.Sprintf("github app %q is referenced by remotes: %s", e.Name, strings.Join(e.Remotes, ", "))
}

const githubAppCols = `name, app_id, installation_id, api_base_url, description, private_key_fingerprint, private_key, created_at, updated_at`

func scanGitHubApp(scanner interface{ Scan(...any) error }, a *models.GitHubApp) error {
	return scanner.Scan(&a.Name, &a.AppID, &a.InstallationID, &a.APIBaseURL, &a.Description,
		&a.PrivateKeyFingerprint, &a.PrivateKey, &a.CreatedAt, &a.UpdatedAt)
}

// GetGitHubApp returns the app including its private key.
func (db *DB) GetGitHubApp(ctx context.Context, name string) (*models.GitHubApp, error) {
	var a models.GitHubApp
	err := scanGitHubApp(db.Pool.QueryRow(ctx, `SELECT `+githubAppCols+` FROM github_apps WHERE name = $1`, name), &a)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGitHubAppNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListGitHubApps returns every app ordered by name; private keys are not loaded.
func (db *DB) ListGitHubApps(ctx context.Context) ([]models.GitHubApp, error) {
	rows, err := db.Pool.Query(ctx, `SELECT name, app_id, installation_id, api_base_url, description,
		private_key_fingerprint, '', created_at, updated_at FROM github_apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := []models.GitHubApp{}
	for rows.Next() {
		var a models.GitHubApp
		if err := scanGitHubApp(rows, &a); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// PutGitHubApp creates or replaces an app, filling a's timestamps. updated_at
// only moves when a field changes, so an identical PUT keeps cached credentials.
func (db *DB) PutGitHubApp(ctx context.Context, a *models.GitHubApp) error {
	return db.Pool.QueryRow(ctx, `
		INSERT INTO github_apps (name, app_id, installation_id, api_base_url, description, private_key_fingerprint, private_key, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, statement_timestamp(), statement_timestamp())
		ON CONFLICT (name) DO UPDATE SET
			app_id = EXCLUDED.app_id,
			installation_id = EXCLUDED.installation_id,
			api_base_url = EXCLUDED.api_base_url,
			description = EXCLUDED.description,
			private_key_fingerprint = EXCLUDED.private_key_fingerprint,
			private_key = EXCLUDED.private_key,
			updated_at = CASE
				WHEN (github_apps.app_id, github_apps.installation_id, github_apps.api_base_url, github_apps.description, github_apps.private_key)
					IS DISTINCT FROM (EXCLUDED.app_id, EXCLUDED.installation_id, EXCLUDED.api_base_url, EXCLUDED.description, EXCLUDED.private_key)
				THEN statement_timestamp() ELSE github_apps.updated_at END
		RETURNING created_at, updated_at
	`, a.Name, a.AppID, a.InstallationID, a.APIBaseURL, a.Description, a.PrivateKeyFingerprint, a.PrivateKey,
	).Scan(&a.CreatedAt, &a.UpdatedAt)
}

// DeleteGitHubApp removes an app, refusing with *GitHubAppInUseError while
// remotes reference it. Locking the app row first makes a concurrent remote
// write that references it wait, then fail its foreign key.
func (db *DB) DeleteGitHubApp(ctx context.Context, name string) error {
	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		var locked string
		err := tx.QueryRow(ctx, `SELECT name FROM github_apps WHERE name = $1 FOR UPDATE`, name).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrGitHubAppNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT name FROM remotes WHERE github_app = $1 ORDER BY name`, name)
		if err != nil {
			return err
		}
		users, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &GitHubAppInUseError{Name: name, Remotes: users}
		}
		_, err = tx.Exec(ctx, `DELETE FROM github_apps WHERE name = $1`, name)
		return err
	})
}

// IsForeignKeyViolation reports whether err is a Postgres foreign key violation.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
