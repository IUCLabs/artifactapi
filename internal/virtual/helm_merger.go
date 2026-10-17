package virtual

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	RegisterMerger(models.PackageHelm, &HelmMerger{})
}

type HelmMerger struct{}

type helmIndex struct {
	APIVersion string                            `yaml:"apiVersion"`
	Entries    map[string][]map[string]yaml.Node `yaml:"entries"`
	Generated  string                            `yaml:"generated,omitempty"`
}

// MergeIndexes keeps each chart version's fields as raw nodes so digest,
// apiVersion and the rest survive the merge untouched; only urls are rewritten.
func (m *HelmMerger) MergeIndexes(members []MemberIndex, proxyBaseURL string) ([]byte, error) {
	merged := &helmIndex{
		APIVersion: "v1",
		Entries:    make(map[string][]map[string]yaml.Node),
	}

	seen := map[string]map[string]bool{}

	for _, member := range members {
		var idx helmIndex
		if err := yaml.Unmarshal(member.Body, &idx); err != nil {
			continue
		}

		budget := provider.MaxAliasExpandedNodes
		for _, chart := range slices.Sorted(maps.Keys(idx.Entries)) {
			versions := idx.Entries[chart]
			if seen[chart] == nil {
				seen[chart] = map[string]bool{}
			}
			for _, raw := range versions {
				ver, err := provider.ExpandYAMLAliases(raw, &budget)
				if err != nil {
					slog.Warn("helm merge: dropping chart entry", "member", member.RemoteName, "chart", chart, "error", err)
					continue
				}
				version := ver["version"].Value
				if seen[chart][version] {
					continue
				}
				seen[chart][version] = true

				if urlsNode, ok := ver["urls"]; ok && proxyBaseURL != "" {
					var urls []string
					if err := urlsNode.Decode(&urls); err != nil {
						continue
					}
					rewriteHelmURLs(urls, member, proxyBaseURL)
					var n yaml.Node
					if err := n.Encode(urls); err != nil {
						return nil, err
					}
					ver["urls"] = n
				}

				merged.Entries[chart] = append(merged.Entries[chart], ver)
			}
		}
	}

	return yaml.Marshal(merged)
}

func rewriteHelmURLs(urls []string, member MemberIndex, proxyBaseURL string) {
	routePrefix := "remote"
	if member.RepoType == "local" {
		routePrefix = "local"
	}
	baseHost := extractHost(member.BaseURL)

	for i, u := range urls {
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			if baseHost != "" && extractHost(u) != baseHost {
				continue
			}
			relPath := extractPathRelativeToBase(u, member.BaseURL)
			urls[i] = fmt.Sprintf("%s/api/v1/%s/%s/%s",
				strings.TrimRight(proxyBaseURL, "/"),
				routePrefix,
				member.RemoteName,
				relPath)
		} else {
			urls[i] = fmt.Sprintf("%s/api/v1/%s/%s/%s",
				strings.TrimRight(proxyBaseURL, "/"),
				routePrefix,
				member.RemoteName,
				u)
		}
	}
}

func extractHost(rawURL string) string {
	idx := strings.Index(rawURL, "://")
	if idx == -1 {
		return ""
	}
	rest := rawURL[idx+3:]
	slashIdx := strings.Index(rest, "/")
	if slashIdx == -1 {
		return rest
	}
	return rest[:slashIdx]
}

func extractPathRelativeToBase(rawURL, baseURL string) string {
	fullPath := extractPath(rawURL)
	basePath := extractPath(baseURL)
	if basePath != "" {
		basePath = strings.TrimRight(basePath, "/") + "/"
		if strings.HasPrefix(fullPath, basePath) {
			return fullPath[len(basePath):]
		}
	}
	return fullPath
}

func extractPath(rawURL string) string {
	idx := strings.Index(rawURL, "://")
	if idx == -1 {
		return rawURL
	}
	rest := rawURL[idx+3:]
	slashIdx := strings.Index(rest, "/")
	if slashIdx == -1 {
		return ""
	}
	return rest[slashIdx+1:]
}
