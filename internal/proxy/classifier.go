package proxy

import (
	"regexp"

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

func compilePatterns(patterns []string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if re, err := regexp.Compile(p); err == nil {
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
