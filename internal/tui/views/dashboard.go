package views

import (
	"fmt"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func FormatBytes(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	b := float64(bytes)
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", b, units[i])
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

func RenderDashboard(stats *models.OverviewStats, remoteCount, virtualCount int) string {
	if stats == nil {
		return "No stats available"
	}

	return fmt.Sprintf(
		"╭─ Dashboard ──────────────────────────────╮\n"+
			"│  Remotes:        %-24d│\n"+
			"│  Cached Objects: %-24d│\n"+
			"│  Storage Used:   %-24s│\n"+
			"│  Dedup Savings:  %-20d blobs │\n"+
			"│  Virtuals:       %-24d│\n"+
			"╰──────────────────────────────────────────╯",
		stats.TotalRemotes,
		stats.TotalObjects,
		FormatBytes(stats.TotalBytes),
		stats.TotalBlobsDeduped,
		virtualCount,
	)
}
