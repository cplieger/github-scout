package github

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/ghsignal"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/slogx/capture"
)

// condServer is an httptest server that serves body with an ETag and answers
// a matching If-None-Match with 304, counting full responses vs
// revalidations. The handler asserts the auth headers ride the conditional
// path exactly as the unconditional one.
type condServer struct {
	srv      *httptest.Server
	lastINM  atomic.Pointer[string]
	etag     string
	body     string
	full     atomic.Int64
	notMod   atomic.Int64
	sendETag bool
}

func newCondServer(t *testing.T, etag, body string) *condServer {
	t.Helper()
	cs := &condServer{etag: etag, body: body, sendETag: true}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		inm := r.Header.Get("If-None-Match")
		cs.lastINM.Store(&inm)
		if inm != "" && inm == cs.etag {
			cs.notMod.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if cs.sendETag {
			w.Header().Set("ETag", cs.etag)
		}
		w.Header().Set("Content-Type", "application/json")
		cs.full.Add(1)
		_, _ = w.Write([]byte(cs.body))
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

const condRepoBody = `[{"name":"keep","owner":{"login":"cplieger"},"private":true,"archived":false}]`

// TestListRepos_conditionalRevalidation pins the whole conditional cycle in
// one process: the first call is unconditional (no If-None-Match), a full
// 200 populates the cache, and the second call replays the ETag, gets a
// 304, and re-serves the identical repo snapshot from the cached items.
func TestListRepos_conditionalRevalidation(t *testing.T) {
	cs := newCondServer(t, `W/"repos-v1"`, condRepoBody)
	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: slog.Default()})
	c.baseURL = cs.srv.URL

	first, err := c.ListRepos(t.Context(), "cplieger")
	if err != nil {
		t.Fatalf("ListRepos #1: %v", err)
	}
	if got := cs.lastINM.Load(); got == nil || *got != "" {
		t.Errorf("first request If-None-Match = %q, want empty (cold cache must not revalidate)", *got)
	}
	second, err := c.ListRepos(t.Context(), "cplieger")
	if err != nil {
		t.Fatalf("ListRepos #2: %v", err)
	}
	if got := cs.lastINM.Load(); got == nil || *got != `W/"repos-v1"` {
		t.Errorf("second request If-None-Match = %v, want the captured ETag", got)
	}
	if cs.full.Load() != 1 || cs.notMod.Load() != 1 {
		t.Errorf("server saw full=%d notModified=%d, want 1 and 1", cs.full.Load(), cs.notMod.Load())
	}
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Errorf("304 snapshot diverges from the 200 snapshot: first=%+v second=%+v", first, second)
	}
}

// TestListCodeScanningAlerts_conditionalRevalidation pins the same cycle for
// the per-repo alerts endpoint, including that an empty alert list is a
// usable cached representation (a 304 re-serves "no alerts", it does not
// force a refetch).
func TestListCodeScanningAlerts_conditionalRevalidation(t *testing.T) {
	cs := newCondServer(t, `W/"alerts-v1"`, `[]`)
	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: slog.Default()})
	c.baseURL = cs.srv.URL
	repo := ghsignal.Repo{Owner: "cplieger", Name: "x"}

	for i := range 2 {
		alerts, err := c.ListCodeScanningAlerts(t.Context(), repo)
		if err != nil {
			t.Fatalf("ListCodeScanningAlerts #%d: %v", i+1, err)
		}
		if len(alerts) != 0 {
			t.Errorf("call #%d returned %d alerts, want 0", i+1, len(alerts))
		}
	}
	if cs.full.Load() != 1 || cs.notMod.Load() != 1 {
		t.Errorf("server saw full=%d notModified=%d, want 1 and 1", cs.full.Load(), cs.notMod.Load())
	}
}

// TestConditional_cachePersistsAcrossClients pins the cross-process contract
// (a daemon restart, or a one-shot trigger after the daemon): a second
// client constructed on the same cache path revalidates with the first
// client's validators and serves the snapshot from the persisted items.
func TestConditional_cachePersistsAcrossClients(t *testing.T) {
	cs := newCondServer(t, `W/"repos-v1"`, condRepoBody)
	path := filepath.Join(t.TempDir(), "cond-cache.json")

	c1 := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: slog.Default(), CondCachePath: path})
	c1.baseURL = cs.srv.URL
	if _, err := c1.ListRepos(t.Context(), "cplieger"); err != nil {
		t.Fatalf("ListRepos (client 1): %v", err)
	}

	c2 := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: slog.Default(), CondCachePath: path})
	c2.baseURL = cs.srv.URL
	repos, err := c2.ListRepos(t.Context(), "cplieger")
	if err != nil {
		t.Fatalf("ListRepos (client 2): %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "keep" {
		t.Errorf("client 2 repos = %+v, want the cached snapshot", repos)
	}
	if cs.full.Load() != 1 || cs.notMod.Load() != 1 {
		t.Errorf("server saw full=%d notModified=%d, want 1 (client 1) and 1 (client 2)", cs.full.Load(), cs.notMod.Load())
	}
}

// TestConditional_corruptCacheFileStartsCold pins the self-heal contract: a
// garbage cache file is tolerated (warn + cold start), the first request is
// unconditional, and the file is rebuilt by the next 200.
func TestConditional_corruptCacheFileStartsCold(t *testing.T) {
	cs := newCondServer(t, `W/"repos-v1"`, condRepoBody)
	path := filepath.Join(t.TempDir(), "cond-cache.json")
	if err := os.WriteFile(path, []byte("{torn garbage"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: slog.Default(), CondCachePath: path})
	c.baseURL = cs.srv.URL
	if _, err := c.ListRepos(t.Context(), "cplieger"); err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if got := cs.lastINM.Load(); got == nil || *got != "" {
		t.Errorf("request after corrupt cache sent If-None-Match %q, want empty", *got)
	}
	if cs.full.Load() != 1 {
		t.Errorf("server saw %d full responses, want 1", cs.full.Load())
	}
}

// TestConditional_noValidatorsMeansNoCaching pins the no-validator branch: a
// server that never sends an ETag/Last-Modified gets an unconditional
// request every time and the cache stores nothing.
func TestConditional_noValidatorsMeansNoCaching(t *testing.T) {
	cs := newCondServer(t, `W/"unused"`, condRepoBody)
	cs.sendETag = false
	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: slog.Default()})
	c.baseURL = cs.srv.URL

	for i := range 2 {
		if _, err := c.ListRepos(t.Context(), "cplieger"); err != nil {
			t.Fatalf("ListRepos #%d: %v", i+1, err)
		}
	}
	if cs.full.Load() != 2 || cs.notMod.Load() != 0 {
		t.Errorf("server saw full=%d notModified=%d, want 2 and 0", cs.full.Load(), cs.notMod.Load())
	}
}

// TestConditional_304WithoutCacheRefetchesOnce pins the defensive branch: an
// out-of-contract upstream answering an unconditional request with 304 gets
// exactly one unconditional refetch, then a hard error — never a loop and
// never a silently empty snapshot.
func TestConditional_304WithoutCacheRefetchesOnce(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: slog.Default()})
	c.baseURL = srv.URL
	_, err := c.ListRepos(t.Context(), "cplieger")
	if err == nil {
		t.Fatal("ListRepos = nil error, want the 304-without-cache fault")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server saw %d requests, want exactly 2 (initial + one refetch)", got)
	}
}

// TestConditional_statusMapping pins the domain-sentinel mapping through the
// conditional door's error family (CheckHTTPStatus classification): a 401
// escalates to ErrTokenInvalid, a 429 to ErrRateLimited, and a 403 stays
// generic — it must NEVER read as token_invalid, or every private repo
// without Advanced Security would page as a dead token.
func TestConditional_statusMapping(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantToken   bool
		wantLimited bool
	}{
		{"401 escalates to token invalid", http.StatusUnauthorized, true, false},
		{"429 escalates to rate limited", http.StatusTooManyRequests, false, true},
		{"403 stays generic", http.StatusForbidden, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", RetryOpts: []httpx.Option{httpx.WithMaxAttempts(1), httpx.WithBaseDelay(time.Millisecond)}, Logger: slog.Default()})
			c.baseURL = srv.URL

			_, err := c.ListRepos(t.Context(), "cplieger")
			if err == nil {
				t.Fatalf("ListRepos = nil error, want HTTP %d surfaced", tt.status)
			}
			if got := errors.Is(err, ghsignal.ErrTokenInvalid); got != tt.wantToken {
				t.Errorf("errors.Is(err, ErrTokenInvalid) = %v, want %v (err=%v)", got, tt.wantToken, err)
			}
			if got := errors.Is(err, ghsignal.ErrRateLimited); got != tt.wantLimited {
				t.Errorf("errors.Is(err, ErrRateLimited) = %v, want %v (err=%v)", got, tt.wantLimited, err)
			}
		})
	}
}

// TestConditional_codeScanning404StillMapsToNoCodeScanning pins that the
// 404 -> ErrNoCodeScanning mapping survives the conditional-door switch (a
// 404 now arrives as *httpx.HTTPStatusError, not GetBytes's *StatusError).
func TestConditional_codeScanning404StillMapsToNoCodeScanning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: slog.Default()})
	c.baseURL = srv.URL

	_, err := c.ListCodeScanningAlerts(t.Context(), ghsignal.Repo{Owner: "cplieger", Name: "x"})
	if !errors.Is(err, ghsignal.ErrNoCodeScanning) {
		t.Errorf("ListCodeScanningAlerts error = %v, want ErrNoCodeScanning", err)
	}
}

// TestConditional_500IsRetried pins the transientStatus wrapper: the
// conditional door retries a plain 500 exactly as the GetBytes door does
// (DoConditional's own classification would treat only 502/503/504 as
// transient), so the scan's one health-flipping call keeps its retries.
func TestConditional_500IsRetried(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", RetryOpts: []httpx.Option{httpx.WithBaseDelay(time.Millisecond)}, Logger: slog.Default()})
	c.baseURL = srv.URL

	if _, err := c.ListRepos(t.Context(), "cplieger"); err != nil {
		t.Fatalf("ListRepos = %v, want the 500 retried into a 200", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server saw %d requests, want 2 (500 then retried 200)", got)
	}
}

// TestCondCache_persistEvictsOldestWhenOverBound pins marshalBounded: a
// persisted payload larger than the slot's read bound evicts
// least-recently-used entries (persisted copy only) until it fits, so the
// file can always be read back whole.
func TestCondCache_persistEvictsOldestWhenOverBound(t *testing.T) {
	entries := make(map[string]cacheEntry)
	big := make([]byte, 1024)
	for i := range big {
		big[i] = 'a'
	}
	// ~70 entries x ~1.1 KiB > 60 KiB bound; UsedAt increases with i so the
	// smallest i is always the eviction victim.
	base := time.Now().Add(-time.Hour)
	for i := range 70 {
		entries[filepath.Join("https://example.test/page", string(rune('A'+i)))] = cacheEntry{
			ETag:   `W/"x"`,
			Items:  append(json.RawMessage(`["`), append(big, '"', ']')...),
			UsedAt: base.Add(time.Duration(i) * time.Second),
		}
	}
	data, err := marshalBounded(entries)
	if err != nil {
		t.Fatalf("marshalBounded: %v", err)
	}
	if len(data) > condCacheMaxBytes {
		t.Errorf("payload = %d bytes, want <= %d", len(data), condCacheMaxBytes)
	}
	if len(entries) == 0 {
		t.Error("eviction removed every entry; want the newest retained")
	}
	newestKey := filepath.Join("https://example.test/page", string(rune('A'+69)))
	if _, ok := entries[newestKey]; !ok {
		t.Error("newest entry was evicted; eviction must drop oldest-first")
	}
}

// TestCondCache_lastModifiedOnlyIsRevalidatable pins that a representation
// validated only by Last-Modified is cached like an ETag'd one. Several GitHub
// endpoints answer without an ETag, and treating those as unrevalidatable
// would spend a full-price GET on every scan forever.
func TestCondCache_lastModifiedOnlyIsRevalidatable(t *testing.T) {
	const (
		reqURL       = "https://example.test/user/repos"
		lastModified = "Fri, 21 Aug 2026 15:35:00 GMT"
	)
	c := newCondCache("", slog.Default())
	c.store(reqURL, httpx.Validators{LastModified: lastModified}, []string{"keep"})

	got := c.validators(reqURL)
	if got.LastModified != lastModified {
		t.Errorf("validators(%q).LastModified = %q, want %q", reqURL, got.LastModified, lastModified)
	}
	if got.ETag != "" {
		t.Errorf("validators(%q).ETag = %q, want empty", reqURL, got.ETag)
	}
	var items []string
	if !c.decodeInto(reqURL, &items) {
		t.Fatalf("decodeInto(%q) = false, want the stored items re-servable on a 304", reqURL)
	}
	if len(items) != 1 || items[0] != "keep" {
		t.Errorf("decodeInto(%q) items = %v, want [keep]", reqURL, items)
	}
}

// TestCondCache_successfulSaveIsSilent pins that a healthy save emits nothing.
// The watcher is read entirely through its log stream, so a line on the
// success path would report a broken cache on every single scan.
func TestCondCache_successfulSaveIsSilent(t *testing.T) {
	logger, rec := capture.New()
	c := newCondCache(filepath.Join(t.TempDir(), "cond-cache.json"), logger)
	c.store("https://example.test/user/repos", httpx.Validators{ETag: `W/"v1"`}, []string{"keep"})

	if rec.Len() != 0 {
		t.Errorf("a successful cache save logged %d line(s) %v, want none", rec.Len(), rec.Messages())
	}
}

// TestCondCache_saveKeepsAnotherProcessesEntries pins the merge under the
// slot's flock: an entry written by a concurrent process after this cache
// loaded — the scheduled daemon racing a hand-exec'd trigger — must survive
// this process's save instead of being overwritten away.
func TestCondCache_saveKeepsAnotherProcessesEntries(t *testing.T) {
	const (
		mine  = "https://example.test/mine"
		other = "https://example.test/other"
	)
	path := filepath.Join(t.TempDir(), "cond-cache.json")
	c := newCondCache(path, slog.Default()) // loads cold: memory holds nothing

	// The other process writes its entry after this cache loaded, so the only
	// copy of it is the one on disk.
	persisted, err := json.Marshal(map[string]cacheEntry{
		other: {ETag: `W/"other"`, Items: json.RawMessage(`["o"]`), UsedAt: time.Now().Add(-time.Hour)},
	})
	if err != nil {
		t.Fatalf("setup: marshal the other process's entry: %v", err)
	}
	if err := os.WriteFile(path, persisted, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	c.store(mine, httpx.Validators{ETag: `W/"mine"`}, []string{"m"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the cache file back: %v", err)
	}
	var merged map[string]cacheEntry
	if err := json.Unmarshal(data, &merged); err != nil {
		t.Fatalf("cache file does not parse after the save: %v (%s)", err, data)
	}
	if _, ok := merged[other]; !ok {
		t.Errorf("save dropped the concurrently written entry %q; file holds %d entry/entries", other, len(merged))
	}
	if _, ok := merged[mine]; !ok {
		t.Errorf("save did not persist this process's entry %q", mine)
	}
}

// TestCondCache_payloadExactlyAtTheBoundIsKeptWhole pins the inclusive edge of
// the persisted-size cap: a payload marshaling to exactly the bound still
// reads back whole, so evicting from it would discard a usable validator for
// no gain.
func TestCondCache_payloadExactlyAtTheBoundIsKeptWhole(t *testing.T) {
	// Sized so the marshaled map lands exactly on the cap. UsedAt is fixed
	// and carries no trailing-zero nanoseconds, so its encoding is a stable
	// width; the guard below fails loudly if the entry shape ever changes.
	const padding = 61339
	const reqURL = "https://example.test/p"
	entries := map[string]cacheEntry{
		reqURL: {
			ETag:   `W/"x"`,
			Items:  json.RawMessage(`["` + strings.Repeat("a", padding) + `"]`),
			UsedAt: time.Date(2026, 8, 21, 15, 35, 0, 123456789, time.UTC),
		},
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("setup: marshal fixture: %v", err)
	}
	if len(raw) != condCacheMaxBytes {
		t.Fatalf("fixture drifted: entry marshals to %d bytes, want exactly %d (adjust padding by %d)",
			len(raw), condCacheMaxBytes, condCacheMaxBytes-len(raw))
	}

	data, err := marshalBounded(entries)
	if err != nil {
		t.Fatalf("marshalBounded: %v", err)
	}
	if _, ok := entries[reqURL]; !ok {
		t.Errorf("a payload of exactly %d bytes was evicted, want it retained", condCacheMaxBytes)
	}
	if len(data) != condCacheMaxBytes {
		t.Errorf("marshalBounded returned %d bytes, want the whole %d-byte payload", len(data), condCacheMaxBytes)
	}
}
