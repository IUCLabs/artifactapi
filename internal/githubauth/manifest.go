package githubauth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// StatusError is a non-success GitHub response; the body is never kept.
type StatusError struct {
	Op     string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("github %s: status %d", e.Op, e.Status)
}

// ManifestConversion is the subset of a manifest conversion response that
// artifactapi keeps. Webhook and OAuth client secrets are never decoded.
type ManifestConversion struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	PEM  string `json:"pem"`
}

// ConvertManifest exchanges a manifest code for the new app's id, slug and
// private key. The endpoint is unauthenticated; the code is single use.
func ConvertManifest(ctx context.Context, client *http.Client, apiBase, code string) (*ManifestConversion, error) {
	u := strings.TrimRight(apiBase, "/") + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	var out ManifestConversion
	if err := githubJSON(ctx, client, http.MethodPost, u, "", "manifest conversion", &out); err != nil {
		return nil, err
	}
	if out.ID <= 0 || out.Slug == "" || out.PEM == "" {
		return nil, errors.New("github manifest conversion: response is missing id, slug or pem")
	}
	if _, err := ParsePrivateKey(out.PEM); err != nil {
		return nil, fmt.Errorf("github manifest conversion: %w", err)
	}
	return &out, nil
}

// InstallationAppID returns the app_id owning installationID, read with the
// app's own JWT, so a 404 means the installation is not this app's.
func InstallationAppID(ctx context.Context, client *http.Client, apiBase string, appID int64, key *rsa.PrivateKey, installationID int64) (int64, error) {
	jwt, err := mintJWT(strconv.FormatInt(appID, 10), key, time.Now())
	if err != nil {
		return 0, fmt.Errorf("github app: mint jwt: %w", err)
	}
	u := strings.TrimRight(apiBase, "/") + "/app/installations/" + strconv.FormatInt(installationID, 10)
	var out struct {
		AppID int64 `json:"app_id"`
	}
	if err := githubJSON(ctx, client, http.MethodGet, u, jwt, "installation lookup", &out); err != nil {
		return 0, err
	}
	return out.AppID, nil
}

func githubJSON(ctx context.Context, client *http.Client, method, u, bearer, op string, out any) error {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Op: op, Status: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("github %s: decode response: %w", op, err)
	}
	return nil
}
