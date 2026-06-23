package virtual

import (
	"fmt"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

type MemberIndex struct {
	RemoteName string
	RepoType   models.RepoType
	Body       []byte
}

type IndexMerger interface {
	MergeIndexes(members []MemberIndex, proxyBaseURL string) ([]byte, error)
}

var mergers = map[models.PackageType]IndexMerger{}

func RegisterMerger(pt models.PackageType, m IndexMerger) {
	mergers[pt] = m
}

func GetMerger(pt models.PackageType) (IndexMerger, error) {
	m, ok := mergers[pt]
	if !ok {
		return nil, fmt.Errorf("no merger registered for package type %q", pt)
	}
	return m, nil
}
