package models

import "time"

type Blob struct {
	ContentHash string    `json:"content_hash"`
	S3Key       string    `json:"s3_key"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

type Artifact struct {
	ID                   int64      `json:"id"`
	RemoteName           string     `json:"remote_name"`
	Path                 string     `json:"path"`
	ContentHash          string     `json:"content_hash"`
	UpstreamETag         string     `json:"upstream_etag,omitempty"`
	UpstreamLastModified *time.Time `json:"upstream_last_modified,omitempty"`
	FirstSeenAt          time.Time  `json:"first_seen_at"`
	LastFetchedAt        time.Time  `json:"last_fetched_at"`
	LastAccessedAt       time.Time  `json:"last_accessed_at"`
	FetchCount           int64      `json:"fetch_count"`
	AccessCount          int64      `json:"access_count"`
	SizeBytes            int64      `json:"size_bytes"`
	ContentType          string     `json:"content_type,omitempty"`
}

type AccessLogEntry struct {
	ID         int64     `json:"id"`
	RemoteName string    `json:"remote_name"`
	Path       string    `json:"path"`
	CacheHit   bool      `json:"cache_hit"`
	SizeBytes  int64     `json:"size_bytes"`
	UpstreamMS int       `json:"upstream_ms"`
	ClientIP   string    `json:"client_ip"`
	CreatedAt  time.Time `json:"created_at"`
}
