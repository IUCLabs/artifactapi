# Mirror-selection benchmarks

These benchmarks (`selection_bench_test.go`) isolate the **mirror load-balancing
selection overhead** — no network, no DB, no Redis. They build a zero-value
`Engine` and call `baseURLAttemptOrder` / `beginAttempt` / `endAttempt`
directly, the same way `leastconn_test.go` and `multibaseurl_test.go` do.

Goal: quantify how much latency the load-balancing strategy (`round_robin` vs
`least_conn`) adds versus a plain single-URL remote, and give a permanent
regression guard.

## Key context: selection is cache-miss-only

`baseURLAttemptOrder` is called from exactly three places — `headUpstream`,
`fetchFromUpstream`, and `checkUpstream` — all on the **upstream / cache-miss
path**. A cache hit returns `Source: "cache"` from `GetArtifact` / `store.Stat`
*before* any selection code runs. So none of the numbers below apply to the hot
cache-hit path: cache hits pay **zero** selection cost regardless of strategy.
The overhead here is paid once per upstream fetch, alongside a network round-trip
measured in milliseconds.

## How to run

```
go test -run=^$ -bench='BaseURLAttemptOrder|BeginEndAttempt' -benchmem \
  -benchtime=1s -count=6 -cpu=8 ./internal/proxy/
```

## Results

Machine: AMD Ryzen 7 4700U (8 threads), linux/amd64, go1.26.5.
`-benchtime=1s -count=6`; figures below are the **median of 6 runs**.

### Sequential (single-goroutine)

| Benchmark                        | ns/op  | B/op | allocs/op |
|----------------------------------|-------:|-----:|----------:|
| BaseURLAttemptOrder_SingleURL    |   ~133 |   16 |         1 |
| BaseURLAttemptOrder_RoundRobin/3 |   ~462 |  120 |         4 |
| BaseURLAttemptOrder_RoundRobin/8 |   ~682 |  280 |         4 |
| BaseURLAttemptOrder_LeastConn/3  |  ~2690 |  474 |        22 |
| BaseURLAttemptOrder_LeastConn/8  | ~15200 | 2688 |       132 |
| BeginEndAttempt (gauge inc/dec)  |   ~514 |  104 |         4 |

### Parallel (`RunParallel`, GOMAXPROCS=8) — ns/op is wall-time across 8 cores

| Benchmark                                 | ns/op | B/op | allocs/op |
|-------------------------------------------|------:|-----:|----------:|
| BaseURLAttemptOrder_RoundRobin_Parallel/3 | ~67.5 |  120 |         4 |
| BaseURLAttemptOrder_RoundRobin_Parallel/8 |  ~130 |  280 |         4 |
| BaseURLAttemptOrder_LeastConn_Parallel/3  |  ~292 |  474 |        22 |
| BaseURLAttemptOrder_LeastConn_Parallel/8  | ~1673 | 2688 |       132 |
| BeginEndAttempt_Parallel                  | ~71.5 |  104 |         4 |

## Reading the numbers

- **Single-URL is a near-no-op** (~133 ns, 1 alloc): the `len(urls) <= 1`
  early return just returns the pool slice. Every non-mirrored remote takes this
  path.
- **round_robin is cheap**: ~462 ns for a 3-mirror pool, ~682 ns for 8. Cost is
  one atomic cursor increment plus building the rotated `[]string`. Allocs are
  constant at 4 (the ordered slice + its backing string headers), size grows
  with pool length.
- **least_conn is more expensive and scales super-linearly**: ~2.7 µs / 22
  allocs at 3 mirrors, ~15 µs / 132 allocs at 8. The cost is the per-call
  `sort.SliceStable`, whose comparator calls `inflightCounter` (a
  `sync.Map.LoadOrStore` with a `remoteName\x00url` string-concat key plus a
  speculative `new(atomic.Int64)`) O(n·log n) times. That is where the alloc
  count and the time come from — not the sort itself. A future optimization
  could snapshot each mirror's load once before sorting; out of scope for this
  measurement PR.
- **beginAttempt/endAttempt** (~514 ns seq, ~72 ns parallel) is one
  `LoadOrStore` + two atomic adds; it only runs for least_conn multi-mirror
  remotes, once per upstream attempt.
- **Under concurrency the atomics/sync.Map do not collapse**: every parallel
  variant reports *lower* ns/op than its sequential twin because work spreads
  across 8 cores (RunParallel reports aggregate wall-time-per-op). No contention
  cliff on the shared rrCounters cursor, the inflight `sync.Map`, or the
  per-mirror `atomic.Int64` gauges.

## Verdict

At the per-request scale that matters (a cache-miss that is *already* doing a
multi-millisecond network fetch), even the worst case here — least_conn across 8
mirrors at ~15 µs — is <1% of a single upstream round-trip, and round_robin
(~0.5 µs) is negligible. The strategy adds no meaningful latency, and it adds
**exactly zero** to the cache-hit hot path because selection never runs there.

## Raw output (all 6 runs)

```
goos: linux
goarch: amd64
pkg: github.com/IUCLabs/artifactapi/internal/proxy
cpu: AMD Ryzen 7 4700U with Radeon Graphics
BenchmarkBaseURLAttemptOrder_SingleURL-8             	 8635875	       138.4 ns/op	      16 B/op	       1 allocs/op
BenchmarkBaseURLAttemptOrder_SingleURL-8             	 9673108	       126.7 ns/op	      16 B/op	       1 allocs/op
BenchmarkBaseURLAttemptOrder_SingleURL-8             	 8001002	       147.3 ns/op	      16 B/op	       1 allocs/op
BenchmarkBaseURLAttemptOrder_SingleURL-8             	11303490	       135.0 ns/op	      16 B/op	       1 allocs/op
BenchmarkBaseURLAttemptOrder_SingleURL-8             	10125138	       132.0 ns/op	      16 B/op	       1 allocs/op
BenchmarkBaseURLAttemptOrder_SingleURL-8             	 8025687	       130.1 ns/op	      16 B/op	       1 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool3-8      	 2706013	       453.2 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool3-8      	 2498718	       444.4 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool3-8      	 2605516	       471.6 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool3-8      	 2799928	       487.6 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool3-8      	 2463375	       418.6 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool3-8      	 2472265	       474.3 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool8-8      	 1741789	       689.4 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool8-8      	 1775467	       602.1 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool8-8      	 1829398	       688.4 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool8-8      	 1781149	       679.3 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool8-8      	 1795680	       599.4 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin/pool8-8      	 1739122	       684.5 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool3-8       	  424184	      2675 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool3-8       	  426796	      2498 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool3-8       	  427116	      2716 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool3-8       	  430540	      2681 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool3-8       	  418156	      2700 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool3-8       	  423009	      2711 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool8-8       	  163642	     14007 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool8-8       	   78501	     15457 ns/op	    2688 B/op	     131 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool8-8       	   76380	     14928 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool8-8       	  183634	     16091 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool8-8       	   73809	     15561 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn/pool8-8       	   74546	     13962 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBeginEndAttempt-8                           	 2350348	       519.7 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt-8                           	 2321659	       514.0 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt-8                           	 2284635	       438.0 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt-8                           	 2287051	       513.9 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt-8                           	 2286481	       520.3 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt-8                           	 2837775	       512.9 ns/op	     104 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool3-8         	16052568	        67.78 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool3-8         	17452791	        66.07 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool3-8         	17549858	        69.25 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool3-8         	18845167	        64.55 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool3-8         	16285608	        69.81 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool3-8         	17382639	        67.12 ns/op	     120 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool8-8         	 9734368	       120.6 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool8-8         	10154736	       133.3 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool8-8         	10061422	       131.8 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool8-8         	10212364	       127.4 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool8-8         	10259030	       132.5 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_RoundRobin_Parallel/pool8-8         	10069576	       122.4 ns/op	     280 B/op	       4 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool3-8          	 4288112	       292.7 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool3-8          	 4009249	       295.9 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool3-8          	 4176378	       291.3 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool3-8          	 4104871	       289.9 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool3-8          	 4245262	       296.4 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool3-8          	 4079778	       290.3 ns/op	     474 B/op	      22 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool8-8          	  748200	      1636 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool8-8          	  763029	      1653 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool8-8          	  663717	      1772 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool8-8          	  739677	      1676 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool8-8          	  763148	      1669 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBaseURLAttemptOrder_LeastConn_Parallel/pool8-8          	  610597	      1684 ns/op	    2688 B/op	     132 allocs/op
BenchmarkBeginEndAttempt_Parallel-8                              	17266058	        69.46 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt_Parallel-8                              	17151303	        72.05 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt_Parallel-8                              	16919542	        74.17 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt_Parallel-8                              	16948015	        72.49 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt_Parallel-8                              	16918693	        69.52 ns/op	     104 B/op	       4 allocs/op
BenchmarkBeginEndAttempt_Parallel-8                              	17376012	        70.99 ns/op	     104 B/op	       4 allocs/op
```
