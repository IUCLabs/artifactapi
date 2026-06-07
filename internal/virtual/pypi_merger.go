package virtual

import (
	"fmt"
	"sort"
	"strings"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	RegisterMerger(models.PackagePyPI, &PyPIMerger{})
}

type PyPIMerger struct{}

func (m *PyPIMerger) MergeIndexes(members []MemberIndex, proxyBaseURL string) ([]byte, error) {
	links := map[string]string{}

	for _, member := range members {
		body := string(member.Body)
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "<a ") {
				continue
			}

			href := extractHref(line)
			text := extractLinkText(line)
			if text == "" {
				continue
			}

			if _, exists := links[text]; exists {
				continue
			}

			if proxyBaseURL != "" && href != "" {
				href = fmt.Sprintf("%s/api/v1/remote/%s/%s",
					strings.TrimRight(proxyBaseURL, "/"),
					member.RemoteName,
					strings.TrimLeft(href, "/"))
			}

			links[text] = href
		}
	}

	keys := make([]string, 0, len(links))
	for k := range links {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html>\n<html><body>\n")
	for _, name := range keys {
		sb.WriteString(fmt.Sprintf("  <a href=\"%s\">%s</a>\n", links[name], name))
	}
	sb.WriteString("</body></html>\n")

	return []byte(sb.String()), nil
}

func extractHref(tag string) string {
	idx := strings.Index(tag, `href="`)
	if idx == -1 {
		return ""
	}
	rest := tag[idx+6:]
	end := strings.Index(rest, `"`)
	if end == -1 {
		return rest
	}
	return rest[:end]
}

func extractLinkText(tag string) string {
	start := strings.Index(tag, ">")
	if start == -1 {
		return ""
	}
	rest := tag[start+1:]
	end := strings.Index(rest, "</a>")
	if end == -1 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}
