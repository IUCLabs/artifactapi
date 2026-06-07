package models

type RemoteStats struct {
	ObjectCount    int64   `json:"object_count"`
	TotalBytes     int64   `json:"total_bytes"`
	HitRate30d     float64 `json:"hit_rate_30d"`
	Requests30d    int64   `json:"requests_30d"`
	BandwidthSaved int64   `json:"bandwidth_saved_30d"`
}

type OverviewStats struct {
	TotalRemotes      int   `json:"total_remotes"`
	TotalObjects      int64 `json:"total_objects"`
	TotalBytes        int64 `json:"total_bytes"`
	TotalBlobsDeduped int64 `json:"total_blobs_deduped"`
	BandwidthSaved30d int64 `json:"bandwidth_saved_30d"`
}

type RemoteHealth struct {
	Status              string `json:"status"` // healthy, degraded, down
	LastError           string `json:"last_error,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}
