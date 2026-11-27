//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	containerHost   = testcontainers.HostInternal
	clientTimeout   = 2 * time.Minute
	missAttempts    = 3
	hitAttempts     = 2
	attemptCooldown = 5 * time.Second
	// deadlineMargin leaves room for cleanup before -timeout panics the binary.
	deadlineMargin = time.Minute
	minAttempt     = 30 * time.Second
)

var serverPort string

// proxyURL is the artifactapi base URL as seen from inside a client container.
func proxyURL() string {
	return "http://" + containerHost + ":" + serverPort
}

type clientRun struct {
	image  string
	script string
	env    map[string]string
	binds  []string
	// tls exposes the TLS front as $AA_TLS and installs its CA at caPath.
	tls     bool
	timeout time.Duration
}

const caPath = "/usr/local/share/ca-certificates/artifactapi-e2e.crt"

func (c clientRun) run(ctx context.Context, timeout time.Duration) (string, error) {
	env := map[string]string{"AA": proxyURL(), "AA_HOST": containerHost}
	for k, v := range c.env {
		env[k] = v
	}
	port, _ := strconv.Atoi(serverPort)
	ports := []int{port}
	var files []testcontainers.ContainerFile
	if c.tls {
		p, _ := strconv.Atoi(tlsPort)
		ports = append(ports, p)
		env["AA_TLS"] = "https://" + containerHost + ":" + tlsPort
		files = append(files, testcontainers.ContainerFile{Reader: bytes.NewReader(tlsCA), ContainerFilePath: caPath, FileMode: 0o644})
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The host tunnel comes up after start, so the script waits for a go signal.
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:           c.image,
			Entrypoint:      []string{"/bin/sh", "-c"},
			Cmd:             []string{"while [ ! -e /tmp/.aa-go ]; do sleep 0.2; done; set -e\n" + c.script},
			Env:             env,
			User:            "0",
			HostAccessPorts: ports,
			Files:           files,
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds, c.binds...)
				if len(c.binds) > 0 {
					hc.SecurityOpt = append(hc.SecurityOpt, "label=disable")
				}
			},
		},
		Started: true,
	})
	if ctr != nil {
		defer func() { _ = ctr.Terminate(context.WithoutCancel(ctx)) }()
	}
	if err != nil {
		return containerLogs(context.WithoutCancel(ctx), ctr), fmt.Errorf("start %s: %w", c.image, err)
	}
	if err := ctr.CopyToContainer(ctx, []byte{}, "/tmp/.aa-go", 0o644); err != nil {
		return containerLogs(context.WithoutCancel(ctx), ctr), fmt.Errorf("signal %s: %w", c.image, err)
	}
	for {
		state, err := ctr.State(ctx)
		if err != nil {
			return containerLogs(context.WithoutCancel(ctx), ctr), fmt.Errorf("%s: %w", c.image, err)
		}
		if !state.Running {
			out := containerLogs(ctx, ctr)
			if state.ExitCode != 0 {
				return out, fmt.Errorf("%s exited %d", c.image, state.ExitCode)
			}
			return out, nil
		}
		select {
		case <-ctx.Done():
			return containerLogs(context.WithoutCancel(ctx), ctr), fmt.Errorf("%s: %w", c.image, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func containerLogs(ctx context.Context, ctr testcontainers.Container) string {
	if ctr == nil {
		return ""
	}
	rc, err := ctr.Logs(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// attemptTimeout caps an attempt at the client timeout and the time left
// before the test binary's deadline; false means too little is left to try.
func attemptTimeout(t *testing.T, c clientRun) (time.Duration, bool) {
	d := clientTimeout
	if c.timeout > 0 {
		d = c.timeout
	}
	if dl, ok := t.Deadline(); ok {
		d = min(d, time.Until(dl)-deadlineMargin)
	}
	return d, d >= minAttempt
}

func runAttempts(t *testing.T, label string, c clientRun, attempts int) {
	t.Helper()
	lastErr := fmt.Errorf("no time left before the test deadline")
	var lastOut string
	for i := 1; i <= attempts; i++ {
		timeout, ok := attemptTimeout(t, c)
		if !ok {
			break
		}
		out, err := c.run(t.Context(), timeout)
		if err == nil {
			return
		}
		lastErr, lastOut = err, out
		t.Logf("%s attempt %d/%d failed: %v\n%s", label, i, attempts, err, tail(out, 20))
		if i < attempts {
			time.Sleep(attemptCooldown)
		}
	}
	t.Fatalf("%s run failed: %v\n%s", label, lastErr, tail(lastOut, 80))
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// cached names artifacts the client must have pulled through a remote.
type cached struct {
	remote string
	path   *regexp.Regexp
}

func listArtifacts(t *testing.T, remote string) []models.Artifact {
	t.Helper()
	resp, err := http.Get(apiURL("/api/v2/remotes/" + remote + "/objects?per_page=5000"))
	if err != nil {
		t.Fatalf("list objects %s: %v", remote, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("list objects %s: status %d: %s", remote, resp.StatusCode, b)
	}
	var out []models.Artifact
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode objects %s: %v", remote, err)
	}
	return out
}

func matching(t *testing.T, c cached) map[string]models.Artifact {
	t.Helper()
	all := listArtifacts(t, c.remote)
	found := map[string]models.Artifact{}
	for _, a := range all {
		if c.path.MatchString(a.Path) {
			found[a.Path] = a
		}
	}
	if len(found) == 0 {
		paths := make([]string, 0, len(all))
		for _, a := range all {
			paths = append(paths, a.Path)
		}
		t.Fatalf("remote %s: no cached object matches %s; cached: %v", c.remote, c.path, paths)
	}
	return found
}

// missThenHit runs the client twice in fresh containers and asserts every
// expected artifact was fetched upstream once and served from cache on the hit.
func missThenHit(t *testing.T, c clientRun, want ...cached) {
	t.Helper()
	runAttempts(t, "miss", c, missAttempts)
	before := make([]map[string]models.Artifact, len(want))
	for i, w := range want {
		before[i] = matching(t, w)
		for p, a := range before[i] {
			if a.FetchCount != 1 {
				t.Errorf("miss %s/%s: fetch_count=%d, want 1", w.remote, p, a.FetchCount)
			}
		}
	}

	runAttempts(t, "hit", c, hitAttempts)
	for i, w := range want {
		after := matching(t, w)
		for p, a := range before[i] {
			got, ok := after[p]
			switch {
			case !ok:
				t.Errorf("hit %s/%s: object disappeared", w.remote, p)
			case got.FetchCount != 1:
				t.Errorf("hit %s/%s: fetch_count=%d, want 1", w.remote, p, got.FetchCount)
			case got.AccessCount <= a.AccessCount:
				t.Errorf("hit %s/%s: access_count=%d, want > %d", w.remote, p, got.AccessCount, a.AccessCount)
			}
		}
	}
}

func remoteJSON(name string, pkgType models.PackageType, baseURL string, extra ...string) string {
	fields := []string{
		fmt.Sprintf(`"name":%q`, name),
		fmt.Sprintf(`"package_type":%q`, pkgType),
		fmt.Sprintf(`"base_url":%q`, baseURL),
		`"mutable_ttl":3600`,
	}
	return "{" + strings.Join(append(fields, extra...), ",") + "}"
}

func withRemote(t *testing.T, name string, pkgType models.PackageType, baseURL string, extra ...string) string {
	t.Helper()
	createRemote(t, remoteJSON(name, pkgType, baseURL, extra...))
	t.Cleanup(func() { deleteRemote(t, name) })
	return name
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(s) }
