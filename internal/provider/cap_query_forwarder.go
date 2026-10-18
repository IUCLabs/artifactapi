package provider

// QueryForwarder is a remote provider whose upstream answers depend on the
// query string (e.g. paginated JSON APIs). Only the named params for path are
// forwarded upstream and kept in the cache key, each as the canonical value
// its validator returns; a value the validator rejects is dropped.
type QueryForwarder interface {
	ForwardedQuery(path string) map[string]QueryValidator
}

// QueryValidator canonicalises a raw query value, reporting false to drop it.
type QueryValidator func(v string) (string, bool)
