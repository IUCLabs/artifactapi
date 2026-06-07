package database

import (
	"context"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func (db *DB) GetVirtual(ctx context.Context, name string) (*models.Virtual, error) {
	row := db.Pool.QueryRow(ctx, `
		SELECT name, package_type, description, members, managed_by, created_at, updated_at
		FROM virtuals WHERE name = $1
	`, name)

	var v models.Virtual
	err := row.Scan(&v.Name, &v.PackageType, &v.Description, &v.Members, &v.ManagedBy, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (db *DB) ListVirtuals(ctx context.Context) ([]models.Virtual, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT name, package_type, description, members, managed_by, created_at, updated_at
		FROM virtuals ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var virtuals []models.Virtual
	for rows.Next() {
		var v models.Virtual
		if err := rows.Scan(&v.Name, &v.PackageType, &v.Description, &v.Members, &v.ManagedBy, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		virtuals = append(virtuals, v)
	}
	return virtuals, rows.Err()
}

func (db *DB) CreateVirtual(ctx context.Context, v *models.Virtual) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO virtuals (name, package_type, description, members, managed_by)
		VALUES ($1, $2, $3, $4, $5)
	`, v.Name, v.PackageType, v.Description, v.Members, v.ManagedBy)
	return err
}

func (db *DB) UpdateVirtual(ctx context.Context, v *models.Virtual) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE virtuals SET
			package_type=$2, description=$3, members=$4, managed_by=$5, updated_at=NOW()
		WHERE name=$1
	`, v.Name, v.PackageType, v.Description, v.Members, v.ManagedBy)
	return err
}

func (db *DB) DeleteVirtual(ctx context.Context, name string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM virtuals WHERE name = $1`, name)
	return err
}
