package models

import "time"

type Virtual struct {
	Name        string      `json:"name"`
	PackageType PackageType `json:"package_type"`
	Description string      `json:"description,omitempty"`
	Members     []string    `json:"members"`
	ManagedBy   string      `json:"managed_by,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}
