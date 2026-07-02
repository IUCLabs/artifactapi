package proxy

import (
	"regexp"
	"sync"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type Classification int

const (
	ClassImmutable Classification = iota
	ClassMutable
	ClassDenied
)

func (c Classification) String() string {
	switch c {
	case ClassImmutable:
		return "immutable"
	case ClassMutable:
		return "mutable"
	case ClassDenied:
		return "denied"
	default:
		return "unknown"
	}
}

type Classifier struct {
	provider provider.Provider
}

func NewClassifier(p provider.Provider) *Classifier {
	return &Classifier{provider: p}
}

func (c *Classifier) Classify(remote models.Remote, path string) Classification {
	if matchesAny(path, compilePatterns(remote.Blocklist)) {
		return ClassDenied
	}

	if len(remote.Patterns) > 0 && !matchesAny(path, compilePatterns(remote.Patterns)) {
		return ClassDenied
	}

	if matchesAny(path, compilePatterns(remote.ImmutablePatterns)) {
		return ClassImmutable
	}

	if matchesAny(path, compilePatterns(remote.MutablePatterns)) {
		return ClassMutable
	}

	if c.provider.Classify(path) == provider.Mutable {
		return ClassMutable
	}

	return ClassImmutable
}

// patternCache memoises regex compilation. Classify runs on every proxied
// request and previously recompiled each remote's pattern lists every time;
// keying by the pattern string lets each distinct pattern compile once and
// then be reused, with no invalidation needed (the pattern text is the key).
// A pattern that fails to compile is cached as a typed nil so we don't retry.
var patternCache sync.Map // map[string]*regexp.Regexp

func compileCached(pattern string) *regexp.Regexp {
	if v, ok := patternCache.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	patternCache.Store(pattern, re)
	return re
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if re := compileCached(p); re != nil {
			compiled = append(compiled, re)
		}
	}
	return compiled
}

func matchesAny(path string, patterns []*regexp.Regexp) bool {
	for _, re := range patterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}
