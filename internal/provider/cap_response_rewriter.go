package provider

// ResponseRewriter marks providers whose RewriteResponse changes mutable
// bodies, so the served length is only known after a fetch.
type ResponseRewriter interface {
	RewritesResponse()
}
