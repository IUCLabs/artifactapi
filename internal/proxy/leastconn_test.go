package proxy

import (
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// baseURLAttemptOrder and the in-flight gauge only touch the engine's sync.Map
// fields, so these tests run against a zero-value Engine without a DB/S3/redis
// stack and can drive the gauge deterministically.

// TestLeastConnPicksLeastLoaded pre-loads one mirror's in-flight gauge and
// asserts a least_conn remote starts its attempt order with the idle mirror.
func TestLeastConnPicksLeastLoaded(t *testing.T) {
	e := &Engine{}
	r := models.Remote{
		Name:           "lc",
		BaseURL:        "https://a.example",
		Mirrorlist:     []string{"https://b.example"},
		MirrorStrategy: models.MirrorStrategyLeastConn,
	}

	// Make A appear busy: least_conn must prefer B regardless of RR rotation.
	e.inflightCounter(r.Name, "https://a.example").Add(3)
	for i := 0; i < 5; i++ {
		order := e.baseURLAttemptOrder(r)
		if len(order) != 2 {
			t.Fatalf("attempt %d: order len = %d, want 2", i, len(order))
		}
		if order[0] != "https://b.example" {
			t.Fatalf("attempt %d: least_conn started with %q, want idle mirror https://b.example", i, order[0])
		}
	}

	// Once B is the busier mirror, the starting pick flips to A.
	e.inflightCounter(r.Name, "https://b.example").Add(10)
	if order := e.baseURLAttemptOrder(r); order[0] != "https://a.example" {
		t.Fatalf("after loading B, least_conn started with %q, want https://a.example", order[0])
	}
}

// TestRoundRobinDefaultUnchanged asserts an unset strategy still rotates the
// starting mirror across the pool and ignores the in-flight gauge.
func TestRoundRobinDefaultUnchanged(t *testing.T) {
	e := &Engine{}
	r := models.Remote{
		Name:       "rr",
		BaseURL:    "https://a.example",
		Mirrorlist: []string{"https://b.example"},
	}

	// Even with A heavily loaded, round-robin must still rotate (not avoid A).
	e.inflightCounter(r.Name, "https://a.example").Add(100)
	starts := map[string]int{}
	for i := 0; i < 4; i++ {
		starts[e.baseURLAttemptOrder(r)[0]]++
	}
	if starts["https://a.example"] == 0 || starts["https://b.example"] == 0 {
		t.Fatalf("round-robin did not rotate starting mirror: %v", starts)
	}
}

// TestLeastConnSingleURLNoOp asserts a single-URL pool yields exactly [base_url]
// and beginAttempt is a no-op there and for round-robin remotes.
func TestLeastConnSingleURLNoOp(t *testing.T) {
	e := &Engine{}
	solo := models.Remote{Name: "solo", BaseURL: "https://a.example", MirrorStrategy: models.MirrorStrategyLeastConn}
	if order := e.baseURLAttemptOrder(solo); len(order) != 1 || order[0] != "https://a.example" {
		t.Fatalf("single-url order = %v, want [base_url]", order)
	}
	if ctr := e.beginAttempt(solo, "https://a.example"); ctr != nil {
		t.Fatal("beginAttempt on single-url pool should be a no-op (nil)")
	}

	rr := models.Remote{Name: "rr2", BaseURL: "https://a.example", Mirrorlist: []string{"https://b.example"}}
	if ctr := e.beginAttempt(rr, "https://a.example"); ctr != nil {
		t.Fatal("beginAttempt on round-robin remote should be a no-op (nil)")
	}
}

// TestBeginEndAttemptGauge asserts the gauge increments on begin and returns to
// zero after endAttempt, so it tracks live in-flight requests.
func TestBeginEndAttemptGauge(t *testing.T) {
	e := &Engine{}
	r := models.Remote{
		Name:           "g",
		BaseURL:        "https://a.example",
		Mirrorlist:     []string{"https://b.example"},
		MirrorStrategy: models.MirrorStrategyLeastConn,
	}
	c1 := e.beginAttempt(r, "https://a.example")
	c2 := e.beginAttempt(r, "https://a.example")
	if got := e.inflightCounter(r.Name, "https://a.example").Load(); got != 2 {
		t.Fatalf("gauge after two begins = %d, want 2", got)
	}
	endAttempt(c1)
	endAttempt(c2)
	if got := e.inflightCounter(r.Name, "https://a.example").Load(); got != 0 {
		t.Fatalf("gauge after matching ends = %d, want 0", got)
	}
	endAttempt(nil) // tolerated
}
