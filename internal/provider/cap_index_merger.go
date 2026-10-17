package provider

type IndexMerger interface {
	MergeIndexes(members []MemberIndex, proxyBaseURL string) ([]byte, error)
}

type MemberIndex struct {
	RemoteName string
	Body       []byte
}
