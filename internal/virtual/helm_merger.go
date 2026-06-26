package virtual

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	RegisterMerger(models.PackageHelm, &HelmMerger{})
}

type HelmMerger struct{}

type helmIndex struct {
	APIVersion string                        `yaml:"apiVersion"`
	Entries    map[string][]helmChartVersion `yaml:"entries"`
	Generated  string                        `yaml:"generated,omitempty"`
}

type helmChartVersion struct {
	Name    string   `yaml:"name"`
	Version string   `yaml:"version"`
	URLs    []string `yaml:"urls"`
	rest    map[string]any
}

func (m *HelmMerger) MergeIndexes(members []MemberIndex, proxyBaseURL string) ([]byte, error) {
	merged := &helmIndex{
		APIVersion: "v1",
		Entries:    make(map[string][]helmChartVersion),
	}

	seen := map[string]map[string]bool{}

	for _, member := range members {
		var idx helmIndex
		if err := yaml.Unmarshal(member.Body, &idx); err != nil {
			continue
		}

		for chart, versions := range idx.Entries {
			if seen[chart] == nil {
				seen[chart] = map[string]bool{}
			}
			for _, ver := range versions {
				key := chart + ":" + ver.Version
				if seen[chart][ver.Version] {
					continue
				}
				seen[chart][ver.Version] = true

				if proxyBaseURL != "" {
					routePrefix := "remote"
					if member.RepoType == "local" {
						routePrefix = "local"
					}
					baseHost := extractHost(member.BaseURL)

					for i, u := range ver.URLs {
						if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
							if baseHost != "" && extractHost(u) != baseHost {
								continue
							}
							ver.URLs[i] = fmt.Sprintf("%s/api/v1/%s/%s/%s",
								strings.TrimRight(proxyBaseURL, "/"),
								routePrefix,
								member.RemoteName,
								extractPath(u))
						} else {
							ver.URLs[i] = fmt.Sprintf("%s/api/v1/%s/%s/%s",
								strings.TrimRight(proxyBaseURL, "/"),
								routePrefix,
								member.RemoteName,
								u)
						}
					}
				}

				merged.Entries[chart] = append(merged.Entries[chart], ver)
				_ = key
			}
		}
	}

	return yaml.Marshal(merged)
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
