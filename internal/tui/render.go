package tui

import (
	"fmt"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/tui/views"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func (m *model) viewDashboard() string {
	return titleStyle.Render("ArtifactAPI Dashboard") + "\n\n" +
		views.RenderDashboard(m.stats, len(m.remotes), len(m.virtuals)) +
		"\n\n" + mutedStyle.Render("Press [2] for remotes, [3] for virtuals")
}

func (m *model) viewRemotesList() string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Remotes") + "\n\n")

	if len(m.remotes) == 0 {
		sb.WriteString(mutedStyle.Render("No remotes configured"))
		return sb.String()
	}

	for i, r := range m.remotes {
		line := fmt.Sprintf("  %-25s %-12s %s", r.Name, r.PackageType, r.Description)
		if i == m.cursor {
			sb.WriteString(selStyle.Render(line))
		} else {
			sb.WriteString(line)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\n" + mutedStyle.Render("j/k navigate · enter detail · esc back"))
	return sb.String()
}

func (m *model) viewRemoteDetail() string {
	var r *remoteView
	for i := range m.remotes {
		if m.remotes[i].Name == m.selectedRemote {
			r = &remoteView{m.remotes[i]}
			break
		}
	}
	if r == nil {
		return mutedStyle.Render("Remote not found")
	}

	var sb strings.Builder
	sb.WriteString(titleStyle.Render(r.Name) + "\n\n")
	sb.WriteString(fmt.Sprintf("  Type:           %s\n", r.PackageType))
	sb.WriteString(fmt.Sprintf("  Base URL:       %s\n", r.BaseURL))
	sb.WriteString(fmt.Sprintf("  Description:    %s\n", r.Description))
	sb.WriteString(fmt.Sprintf("  Immutable TTL:  %s\n", ttlStr(r.ImmutableTTL)))
	sb.WriteString(fmt.Sprintf("  Mutable TTL:    %ds\n", r.MutableTTL))
	sb.WriteString(fmt.Sprintf("  Revalidation:   %v\n", r.CheckMutable))
	sb.WriteString(fmt.Sprintf("  Stale on Error: %v\n", r.StaleOnError))
	fmt.Fprintf(&sb, "  Serve Stale:    %v\n", r.ServeStale)
	fmt.Fprintf(&sb, "  Cache Repodata: %v\n", r.CacheRepodata)

	if len(r.Patterns) > 0 {
		sb.WriteString(fmt.Sprintf("  Patterns:       %s\n", strings.Join(r.Patterns, ", ")))
	}
	if len(r.Blocklist) > 0 {
		sb.WriteString(fmt.Sprintf("  Blocklist:      %s\n", strings.Join(r.Blocklist, ", ")))
	}
	if r.ManagedBy != "" {
		sb.WriteString(fmt.Sprintf("  Managed by:     %s\n", r.ManagedBy))
	}

	sb.WriteString("\n" + mutedStyle.Render("enter → browse objects · esc back"))
	return sb.String()
}

func (m *model) viewObjectsList() string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render(fmt.Sprintf("Objects: %s (page %d)", m.selectedRemote, m.page)) + "\n\n")

	if len(m.objects) == 0 {
		sb.WriteString(mutedStyle.Render("No cached objects"))
		return sb.String()
	}

	for i, a := range m.objects {
		size := views.FormatBytes(a.SizeBytes)
		line := fmt.Sprintf("  %-50s %10s  %5d hits", truncate(a.Path, 50), size, a.AccessCount)
		if i == m.cursor {
			sb.WriteString(selStyle.Render(line))
		} else {
			sb.WriteString(line)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\n" + mutedStyle.Render("j/k navigate · esc back"))
	return sb.String()
}

func (m *model) viewVirtualsList() string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Virtual Repositories") + "\n\n")

	if len(m.virtuals) == 0 {
		sb.WriteString(mutedStyle.Render("No virtual repositories configured"))
		return sb.String()
	}

	for i, v := range m.virtuals {
		line := fmt.Sprintf("  %-25s %-12s %d members  %s",
			v.Name, v.PackageType, len(v.Members), v.Description)
		if i == m.cursor {
			sb.WriteString(selStyle.Render(line))
		} else {
			sb.WriteString(line)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\n" + mutedStyle.Render("j/k navigate · esc back"))
	return sb.String()
}

type remoteView struct {
	models.Remote
}

func ttlStr(ttl int) string {
	if ttl == 0 {
		return "forever"
	}
	return fmt.Sprintf("%ds", ttl)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
