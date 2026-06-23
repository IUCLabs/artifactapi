package virtual

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type Engine struct {
	db          *database.DB
	proxyEngine *proxy.Engine
}

func NewEngine(db *database.DB, proxyEngine *proxy.Engine) *Engine {
	return &Engine{db: db, proxyEngine: proxyEngine}
}

func (e *Engine) Fetch(ctx context.Context, virt models.Virtual, path string, proxyBaseURL string) ([]byte, string, error) {
	merger, err := GetMerger(virt.PackageType)
	if err != nil {
		return nil, "", fmt.Errorf("unsupported virtual type %q: %w", virt.PackageType, err)
	}

	members, err := e.fetchMemberIndexes(ctx, virt, path)
	if err != nil {
		return nil, "", err
	}

	if len(members) == 0 {
		return nil, "", fmt.Errorf("no members reachable for virtual %q", virt.Name)
	}

	merged, err := merger.MergeIndexes(members, proxyBaseURL)
	if err != nil {
		return nil, "", fmt.Errorf("merge indexes: %w", err)
	}

	contentType := "application/octet-stream"
	switch virt.PackageType {
	case models.PackageHelm:
		contentType = "text/yaml"
	case models.PackagePyPI:
		contentType = "text/html"
	}

	return merged, contentType, nil
}

func (e *Engine) fetchMemberIndexes(ctx context.Context, virt models.Virtual, path string) ([]MemberIndex, error) {
	type result struct {
		index MemberIndex
		err   error
	}

	results := make([]result, len(virt.Members))
	var wg sync.WaitGroup

	for i, memberName := range virt.Members {
		wg.Add(1)
		go func(idx int, name string) {
			defer wg.Done()

			remote, err := e.db.GetRemote(ctx, name)
			if err != nil {
				results[idx] = result{err: fmt.Errorf("remote %q: %w", name, err)}
				return
			}

			if remote.RepoType == models.RepoTypeLocal {
				body, err := e.fetchLocalIndex(ctx, *remote, path)
				if err != nil {
					results[idx] = result{err: fmt.Errorf("local index %q: %w", name, err)}
					return
				}
				results[idx] = result{index: MemberIndex{RemoteName: name, RepoType: remote.RepoType, Body: body}}
				return
			}

			prov, err := provider.Get(remote.PackageType)
			if err != nil {
				results[idx] = result{err: fmt.Errorf("provider %q: %w", remote.PackageType, err)}
				return
			}

			fetchResult, err := e.proxyEngine.Fetch(ctx, *remote, path, prov)
			if err != nil {
				results[idx] = result{err: fmt.Errorf("fetch %q/%s: %w", name, path, err)}
				return
			}
			defer fetchResult.Reader.Close()

			body, err := io.ReadAll(fetchResult.Reader)
			if err != nil {
				results[idx] = result{err: fmt.Errorf("read %q: %w", name, err)}
				return
			}

			results[idx] = result{index: MemberIndex{RemoteName: name, RepoType: remote.RepoType, Body: body}}
		}(i, memberName)
	}

	wg.Wait()

	var members []MemberIndex
	for _, r := range results {
		if r.err != nil {
			slog.Warn("virtual member fetch failed", "error", r.err)
			continue
		}
		members = append(members, r.index)
	}

	return members, nil
}

func (e *Engine) fetchLocalIndex(ctx context.Context, remote models.Remote, path string) ([]byte, error) {
	prov, err := provider.Get(remote.PackageType)
	if err != nil {
		return nil, fmt.Errorf("no provider for %q: %w", remote.PackageType, err)
	}

	indexer, ok := prov.(provider.LocalIndexer)
	if !ok {
		return nil, fmt.Errorf("provider %q does not support local index generation", remote.PackageType)
	}

	return indexer.GenerateLocalIndex(ctx, e.db, remote.Name, path)
}
