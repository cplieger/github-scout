package ghquota

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// answer is one response GitHub sent: its status and rate-limit headers in
// name, value pairs.
func answer(t *testing.T, m *Meter, status int, header ...string) {
	t.Helper()
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	next := roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: h, Body: http.NoBody}, nil
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.test/", http.NoBody)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	resp, err := m.Observe(next).RoundTrip(req)
	if err != nil {
		t.Fatalf("Observe passed on %d as error %v, want the response", status, err)
	}
	resp.Body.Close()
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func armed(now time.Time) *Meter {
	m := New(func() time.Time { return now })
	m.Arm()
	return m
}

func reset(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }

func TestAdmit_refuses_every_read_after_a_403_that_is_no_throttle_until_the_next_scan(t *testing.T) {
	m := armed(t0)
	answer(t, m, http.StatusForbidden, "X-RateLimit-Remaining", "4000")
	if err := m.Admit(); !errors.Is(err, forge.ErrRefused) {
		t.Errorf("Admit after a 403 with budget left = %v, want forge.ErrRefused", err)
	}
	m.BeginScan()
	if err := m.Admit(); err != nil {
		t.Errorf("Admit in the next scan = %v, want nil", err)
	}
}

func TestAdmit_a_throttle_403_trips_no_breaker(t *testing.T) {
	for name, header := range map[string][]string{
		"retry_after":    {"X-RateLimit-Remaining", "4000", "Retry-After", "60"},
		"budget_spent":   {"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset(t0.Add(time.Hour))},
		"retry_no_count": {"Retry-After", "60"},
	} {
		t.Run(name, func(t *testing.T) {
			m := armed(t0)
			answer(t, m, http.StatusForbidden, header...)
			if err := m.Admit(); errors.Is(err, forge.ErrRefused) {
				t.Errorf("Admit after a throttle 403 = %v, want no refusal: the client reports the rate limit and stops itself", err)
			}
		})
	}
}

func TestAdmit_holds_reads_at_the_reserve_until_the_window_renews(t *testing.T) {
	now := t0
	m := New(func() time.Time { return now })
	m.Arm()
	answer(t, m, http.StatusOK, "X-RateLimit-Remaining", strconv.Itoa(forge.ReadReserve+1), "X-RateLimit-Reset", reset(t0.Add(time.Hour)))
	if err := m.Admit(); err != nil {
		t.Fatalf("Admit above the reserve = %v, want nil", err)
	}
	answer(t, m, http.StatusOK, "X-RateLimit-Remaining", strconv.Itoa(forge.ReadReserve), "X-RateLimit-Reset", reset(t0.Add(time.Hour)))
	if err := m.Admit(); !errors.Is(err, forge.ErrReadDeferred) {
		t.Errorf("Admit at the reserve = %v, want forge.ErrReadDeferred", err)
	}
	now = t0.Add(time.Hour)
	if err := m.Admit(); err != nil || m.Budget().Known() {
		t.Errorf("Admit once the window renewed = %v with budget %+v, want nil and the budget unknown", err, m.Budget())
	}
}

func TestBudget_is_the_shared_rest_pool_whatever_the_other_pools_report(t *testing.T) {
	m := armed(t0)
	answer(t, m, http.StatusOK, "X-RateLimit-Remaining", "4000", "X-RateLimit-Reset", reset(t0.Add(time.Hour)))
	answer(t, m, http.StatusOK, "X-RateLimit-Remaining", "12", "X-RateLimit-Resource", "graphql", "X-RateLimit-Reset", reset(t0.Add(time.Hour)))
	if b := m.Budget(); b.Remaining != 4000 {
		t.Errorf("Budget after a core and a graphql answer = %+v, want core's 4000: the REST routes of both clients share it", b)
	}
	if err := m.Admit(); err != nil {
		t.Errorf("Admit with graphql at 12 = %v, want nil: forgeapi's governor holds its own document pool", err)
	}
	answer(t, m, http.StatusOK, "X-RateLimit-Remaining", "3990", "X-RateLimit-Resource", "core", "X-RateLimit-Reset", reset(t0.Add(time.Hour)))
	if b := m.Budget(); b.Remaining != 3990 {
		t.Errorf("Budget after a core answer naming its pool = %+v, want 3990", b)
	}
}

func TestBudget_a_response_with_no_count_charges_the_known_pool_one(t *testing.T) {
	m := armed(t0)
	answer(t, m, http.StatusBadGateway)
	if m.Budget().Known() {
		t.Fatalf("Budget after an answer with no count on a fresh meter = %+v, want unknown", m.Budget())
	}
	answer(t, m, http.StatusOK, "X-RateLimit-Remaining", "10", "X-RateLimit-Reset", reset(t0.Add(time.Hour)))
	answer(t, m, http.StatusBadGateway)
	if b := m.Budget(); b.Remaining != 9 {
		t.Errorf("Budget after an answer with no count = %+v, want 9: the request was sent", b)
	}
}

func TestObserve_an_unarmed_meter_records_nothing(t *testing.T) {
	m := New(nil)
	answer(t, m, http.StatusForbidden, "X-RateLimit-Remaining", "1")
	if err := m.Admit(); err != nil || m.Budget().Known() {
		t.Errorf("an unarmed meter after a 403 = Admit %v, budget %+v, want nil and unknown: it is no GitHub connection's", err, m.Budget())
	}
}

func TestObserve_passes_a_transport_error_on_and_charges_it(t *testing.T) {
	m := armed(t0)
	answer(t, m, http.StatusOK, "X-RateLimit-Remaining", "10", "X-RateLimit-Reset", reset(t0.Add(time.Hour)))
	failing := roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") })
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.test/", http.NoBody)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	resp, err := m.Observe(failing).RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || resp != nil {
		t.Errorf("Observe over a failing transport = %v, %v, want its error passed on", resp, err)
	}
	if b := m.Budget(); b.Remaining != 9 {
		t.Errorf("Budget after a request that got no answer = %+v, want 9", b)
	}
}

// through sends one response with status and headers through the observer
// wrap installs.
func through(t *testing.T, wrap func(http.RoundTripper) http.RoundTripper, status int, header ...string) {
	t.Helper()
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	next := roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: h, Body: http.NoBody}, nil
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.test/", http.NoBody)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	resp, err := wrap(next).RoundTrip(req)
	if err != nil {
		t.Fatalf("observer passed on %d as error %v, want the response", status, err)
	}
	resp.Body.Close()
}

func TestObserveAttempt_charges_each_attempt_and_ObserveRequest_judges_the_answer(t *testing.T) {
	m := armed(t0)
	answer(t, m, http.StatusOK, "X-RateLimit-Remaining", "4000", "X-RateLimit-Reset", reset(t0.Add(time.Hour)))
	through(t, m.ObserveAttempt, http.StatusBadGateway)
	through(t, m.ObserveAttempt, http.StatusOK)
	through(t, m.ObserveRequest, http.StatusOK)
	if b := m.Budget(); b.Remaining != 3998 {
		t.Errorf("Budget after two attempts of one request = %+v, want 3998: each attempt is charged once, the request not again", b)
	}
	through(t, m.ObserveAttempt, http.StatusForbidden, "X-RateLimit-Remaining", "3997")
	if err := m.Admit(); err != nil {
		t.Errorf("Admit after an attempt answered 403 = %v, want nil: the request's answer decides", err)
	}
	through(t, m.ObserveRequest, http.StatusForbidden, "X-RateLimit-Remaining", "3997")
	if err := m.Admit(); !errors.Is(err, forge.ErrRefused) {
		t.Errorf("Admit after a request answered 403 = %v, want forge.ErrRefused", err)
	}
	if b := m.Budget(); b.Remaining != 3997 {
		t.Errorf("Budget after the 403 = %+v, want the 3997 its attempt reported", b)
	}
}
