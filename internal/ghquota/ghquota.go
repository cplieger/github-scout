// Package ghquota is the one account of a GitHub connection's request quota
// that both of its clients, forgeapi's and the REST client for the routes
// forgeapi does not model, draw on. It observes every response either sends,
// keeps each rate-limit pool GitHub names, and holds the scan's refusal
// breaker.
package ghquota

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
)

// corePool is the pool a response naming none drew on, and the one the REST
// routes of both clients share.
const corePool = "core"

// Meter is one GitHub connection's quota. It observes nothing until Arm, so
// a connection that turns out not to be GitHub leaves it inert. Its methods
// are safe for concurrent use.
type Meter struct {
	pools   map[string]forge.Budget
	now     func() time.Time
	mu      sync.Mutex
	armed   bool
	refused bool
}

// New returns an unarmed meter reading the time from now, time.Now when nil.
func New(now func() time.Time) *Meter {
	if now == nil {
		now = time.Now
	}
	return &Meter{pools: map[string]forge.Budget{}, now: now}
}

// Arm starts the meter on a connection that answered as GitHub.
func (m *Meter) Arm() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.armed = true
}

// BeginScan clears the breaker, so each scan sends until GitHub refuses one
// of its reads.
func (m *Meter) BeginScan() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refused = false
}

// Admit reports whether a read may be sent: forge.ErrRefused once GitHub has
// refused one of the scan's reads with a 403 that is no throttle, which
// GitHub answers for a missing permission and for a secondary limit alike
// and asks a limited client to stop on
// (https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api),
// and forge.ErrReadDeferred while the shared REST pool is at the read
// reserve.
func (m *Meter) Admit() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.refused:
		return forge.ErrRefused
	case m.pool(corePool).Reserved():
		return forge.ErrReadDeferred
	}
	return nil
}

// Budget is the shared REST pool's budget as the last response reported it,
// unknown once its window renews.
func (m *Meter) Budget() forge.Budget {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pool(corePool)
}

// pool is the named pool's budget; m.mu must be held.
func (m *Meter) pool(name string) forge.Budget {
	b, ok := m.pools[name]
	if !ok || (!b.Reset.IsZero() && !m.now().Before(b.Reset)) {
		return forge.Budget{Remaining: -1}
	}
	return b
}

// Observe wraps next so the meter accounts every response it carries and
// trips the breaker on a refusal, for a client that sends each attempt
// through it. It passes every request on.
func (m *Meter) Observe(next http.RoundTripper) http.RoundTripper {
	return observer{next: next, see: func(r *http.Response) { m.see(r, true, true) }}
}

// ObserveAttempt wraps next so the meter accounts each attempt it carries, a
// retry included, the position beneath forgeapi's retry layer.
func (m *Meter) ObserveAttempt(next http.RoundTripper) http.RoundTripper {
	return observer{next: next, see: func(r *http.Response) { m.see(r, true, false) }}
}

// ObserveRequest wraps next so a request's final answer, a 403 that is no
// throttle, trips the breaker, the position above forgeapi's retry layer.
func (m *Meter) ObserveRequest(next http.RoundTripper) http.RoundTripper {
	return observer{next: next, see: func(r *http.Response) { m.see(r, false, true) }}
}

type observer struct {
	next http.RoundTripper
	see  func(*http.Response)
}

func (o observer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.next.RoundTrip(req)
	o.see(resp)
	return resp, err
}

// see accounts one sent attempt when account is set: the pool GitHub's
// rate-limit headers name takes the remaining count and reset they report,
// else the known REST pool loses one. When judge is set, a 403 that is no
// throttle trips the breaker.
func (m *Meter) see(resp *http.Response, account, judge bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.armed {
		return
	}
	if judge && resp != nil && resp.StatusCode == http.StatusForbidden && !Throttled(resp.Header) {
		m.refused = true
	}
	if !account {
		return
	}
	if resp == nil {
		m.spendOne()
		return
	}
	n, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if err != nil || n < 0 {
		m.spendOne()
		return
	}
	name := resp.Header.Get("X-RateLimit-Resource")
	if name == "" {
		name = corePool
	}
	b := forge.Budget{Remaining: n}
	if s, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		b.Reset = time.Unix(s, 0).UTC()
	}
	m.pools[name] = b
}

// spendOne charges a response that reported no pool to the known REST pool.
func (m *Meter) spendOne() {
	if b := m.pool(corePool); b.Known() {
		b.Remaining = max(b.Remaining-1, 0)
		m.pools[corePool] = b
	}
}

// Throttled reports a 403 GitHub sends for a rate limit: X-RateLimit-Remaining
// at 0, or a Retry-After header.
func Throttled(h http.Header) bool {
	n, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	return (err == nil && n <= 0) || h.Get("Retry-After") != ""
}
