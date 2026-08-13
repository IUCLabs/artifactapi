package proxy

import (
	"fmt"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// These benchmarks isolate the mirror-selection overhead only: they construct a
// zero-value Engine (no DB/S3/redis) and call baseURLAttemptOrder /
// beginAttempt / endAttempt directly, mirroring leastconn_test.go and
// multibaseurl_test.go. This quantifies how much latency the load-balancing
// strategy (round_robin vs least_conn) adds versus a single-URL remote. Note
// that in the live proxy this selection runs only on the cache-miss/upstream
// path; a cache hit never calls it.

// mirrorPool builds a remote with n upstreams (base_url + n-1 mirrorlist
// entries) under the given strategy.
func mirrorPool(name, strategy string, n int) models.Remote {
	r := models.Remote{
		Name:           name,
		BaseURL:        "https://mirror0.example/repo",
		MirrorStrategy: strategy,
	}
	for i := 1; i < n; i++ {
		r.Mirrorlist = append(r.Mirrorlist, fmt.Sprintf("https://mirror%d.example/repo", i))
	}
	return r
}

// skewInflight sets an ascending in-flight load across the pool so least_conn's
// stable sort has real work to do (mirror0 busiest, last mirror idle).
func skewInflight(e *Engine, r models.Remote) {
	pool := r.UpstreamPool()
	for i, u := range pool {
		e.inflightCounter(r.Name, u).Add(int64(len(pool) - i))
	}
}

// BenchmarkBaseURLAttemptOrder_SingleURL measures the early-return no-op path
// (pool of 1): the branch that preserves original single-attempt behavior and
// must add effectively zero overhead. This is the same code the cache-miss path
// takes for every non-mirrored remote.
func BenchmarkBaseURLAttemptOrder_SingleURL(b *testing.B) {
	e := &Engine{}
	r := models.Remote{Name: "solo", BaseURL: "https://mirror0.example/repo"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.baseURLAttemptOrder(r)
	}
}

// BenchmarkBaseURLAttemptOrder_RoundRobin measures the default strategy: rotate
// the starting mirror by an atomic cursor and materialize the ordered slice. No
// in-flight sort.
func BenchmarkBaseURLAttemptOrder_RoundRobin(b *testing.B) {
	for _, n := range []int{3, 8} {
		b.Run(fmt.Sprintf("pool%d", n), func(b *testing.B) {
			e := &Engine{}
			r := mirrorPool("rr", models.MirrorStrategyRoundRobin, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = e.baseURLAttemptOrder(r)
			}
		})
	}
}

// BenchmarkBaseURLAttemptOrder_LeastConn measures the least_conn strategy: RR
// rotation plus a stable sort of the pool by the atomic in-flight gauges. Skew
// is preloaded so the sort compares distinct loads.
func BenchmarkBaseURLAttemptOrder_LeastConn(b *testing.B) {
	for _, n := range []int{3, 8} {
		b.Run(fmt.Sprintf("pool%d", n), func(b *testing.B) {
			e := &Engine{}
			r := mirrorPool("lc", models.MirrorStrategyLeastConn, n)
			skewInflight(e, r)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = e.baseURLAttemptOrder(r)
			}
		})
	}
}

// BenchmarkBeginEndAttempt measures the gauge inc/dec pair that brackets each
// least_conn upstream attempt (LoadOrStore + atomic add, then atomic add back).
func BenchmarkBeginEndAttempt(b *testing.B) {
	e := &Engine{}
	r := mirrorPool("g", models.MirrorStrategyLeastConn, 3)
	url := r.UpstreamPool()[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctr := e.beginAttempt(r, url)
		endAttempt(ctr)
	}
}

// BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel surfaces atomic-cursor
// contention on the shared rrCounters entry under concurrent selection.
func BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel(b *testing.B) {
	for _, n := range []int{3, 8} {
		b.Run(fmt.Sprintf("pool%d", n), func(b *testing.B) {
			e := &Engine{}
			r := mirrorPool("rrp", models.MirrorStrategyRoundRobin, n)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_ = e.baseURLAttemptOrder(r)
				}
			})
		})
	}
}

// BenchmarkBaseURLAttemptOrder_LeastConn_Parallel surfaces sync.Map read
// contention on the in-flight gauges plus the per-call sort under concurrency.
func BenchmarkBaseURLAttemptOrder_LeastConn_Parallel(b *testing.B) {
	for _, n := range []int{3, 8} {
		b.Run(fmt.Sprintf("pool%d", n), func(b *testing.B) {
			e := &Engine{}
			r := mirrorPool("lcp", models.MirrorStrategyLeastConn, n)
			skewInflight(e, r)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_ = e.baseURLAttemptOrder(r)
				}
			})
		})
	}
}

// BenchmarkBeginEndAttempt_Parallel exercises the gauge inc/dec pair under
// concurrency: all goroutines hammer the same atomic.Int64, the realistic
// hot-mirror case, to surface counter contention.
func BenchmarkBeginEndAttempt_Parallel(b *testing.B) {
	e := &Engine{}
	r := mirrorPool("gp", models.MirrorStrategyLeastConn, 3)
	url := r.UpstreamPool()[0]
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctr := e.beginAttempt(r, url)
			endAttempt(ctr)
		}
	})
}
