package github

import (
	"encoding/json"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/scheduler/v3"
)

// condCacheRetention bounds how long a cache entry survives without being
// refreshed by a 200. A URL that keeps revalidating (304s) has its UsedAt
// refreshed in memory but persisted only when some 200 elsewhere triggers a
// save, so a long-lived stable entry can look stale on disk and be pruned —
// the cost is one full-price refetch after a restart, never a correctness
// fault (validators self-heal per httpx.DoConditional's contract).
const condCacheRetention = 14 * 24 * time.Hour

// condCacheMaxBytes caps the persisted payload safely below
// scheduler.SlotFile's 64 KiB read bound: a larger write would read back
// truncated next start, fail the JSON parse, and cold-start the cache every
// time. When the map outgrows the cap, persist drops least-recently-used
// entries from the PERSISTED copy (memory keeps everything) until it fits.
const condCacheMaxBytes = 60 << 10

// cacheEntry is one cached conditional-GET representation: the validators a
// 200 supplied plus the decoded item subset they validate, so a later 304
// can re-serve the items without a body. Items holds the marshaled page
// subset (e.g. []apiRepo), not GitHub's raw body — the raw body is an order
// of magnitude larger and would blow the slot bound immediately.
type cacheEntry struct {
	UsedAt       time.Time       `json:"used_at"`
	ETag         string          `json:"etag,omitempty"`
	LastModified string          `json:"last_modified,omitempty"`
	Items        json.RawMessage `json:"items"`
}

// condCache is the client's conditional-request cache, keyed by request URL.
// In-memory always; persisted through a flock'd scheduler.SlotFile when a
// path is configured (the seen-runs.json shape), so daemon restarts and
// one-shot `trigger` processes revalidate instead of re-downloading. Like
// the run dedup set, persistence is a best-effort optimization, never a
// correctness dependency: any lost entry costs one full-price GET.
type condCache struct {
	entries map[string]cacheEntry
	slot    *scheduler.SlotFile // nil = in-memory only (tests)
	logger  *slog.Logger
	mu      sync.Mutex
}

// newCondCache builds the cache, loading the persisted entries when path is
// non-empty. A missing, unreadable, corrupt, or truncated-by-the-slot-bound
// file starts the cache cold (each URL's first request is unconditional).
func newCondCache(path string, logger *slog.Logger) *condCache {
	c := &condCache{entries: make(map[string]cacheEntry), logger: logger}
	if path == "" {
		return c
	}
	c.slot = scheduler.NewSlotFile(path)
	c.load()
	return c
}

// load reads the persisted entries (the SlotFile read idiom: a Mutate whose
// fn returns its argument), dropping entries past retention.
func (c *condCache) load() {
	data, err := c.slot.Mutate(func(before []byte) []byte { return before })
	if err != nil {
		c.logger.Warn("conditional cache unreadable; starting cold", "error", err)
		return
	}
	if len(data) == 0 {
		return // first use: the slot was just created empty
	}
	var entries map[string]cacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		c.logger.Warn("conditional cache corrupt; starting cold", "error", err)
		return
	}
	now := time.Now()
	for url, e := range entries {
		if now.Sub(e.UsedAt) <= condCacheRetention {
			c.entries[url] = e
		}
	}
}

// validators returns the stored validators for url, zero when there is no
// cached representation — per DoConditional's contract, an empty cache must
// force a full 200 rather than being eligible for a 304 with nothing to
// reuse. An entry always carries the items its validators validate (store
// records both or nothing), so a non-zero return implies decodeInto can
// serve the 304.
func (c *condCache) validators(url string) httpx.Validators {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[url]
	if !ok {
		return httpx.Validators{}
	}
	return httpx.Validators{ETag: e.ETag, LastModified: e.LastModified}
}

// decodeInto decodes the cached items for url into out, reporting whether a
// usable representation existed. A corrupt entry (persisted bytes that no
// longer unmarshal) is dropped so the caller's unconditional refetch
// replaces it.
func (c *condCache) decodeInto(url string, out any) bool {
	c.mu.Lock()
	e, ok := c.entries[url]
	if ok {
		e.UsedAt = time.Now()
		c.entries[url] = e
	}
	c.mu.Unlock()
	if !ok || len(e.Items) == 0 {
		return false
	}
	if err := json.Unmarshal(e.Items, out); err != nil {
		c.logger.Warn("conditional cache entry corrupt; dropping", "error", err)
		c.mu.Lock()
		delete(c.entries, url)
		c.mu.Unlock()
		return false
	}
	return true
}

// store records a 200's fresh validators plus the decoded items they
// validate, and persists the cache. A response without validators is not
// revalidatable — any previous entry is dropped so a stale validator can
// never be replayed against a URL that stopped serving ETags.
func (c *condCache) store(url string, v httpx.Validators, items any) {
	if v.ETag == "" && v.LastModified == "" {
		c.mu.Lock()
		delete(c.entries, url)
		c.mu.Unlock()
		return
	}
	data, err := json.Marshal(items)
	if err != nil {
		// items just came out of json.Unmarshal, so this cannot realistically
		// fail; degrade to uncached rather than surfacing an error.
		c.logger.Warn("conditional cache marshal failed; entry not cached", "error", err)
		return
	}
	c.mu.Lock()
	c.entries[url] = cacheEntry{
		ETag:         v.ETag,
		LastModified: v.LastModified,
		Items:        data,
		UsedAt:       time.Now(),
	}
	snapshot := maps.Clone(c.entries)
	c.mu.Unlock()
	c.persist(snapshot)
}

// persist merges snapshot into the slot under its flock — per URL the newer
// UsedAt wins, so a concurrent writer (the daemon racing a hand-exec'd
// trigger) never loses fresher validators to last-writer-wins — prunes
// entries past retention, and bounds the payload under the slot's read
// limit. Best-effort: a failure is logged, never surfaced (the cache is an
// optimization).
func (c *condCache) persist(snapshot map[string]cacheEntry) {
	if c.slot == nil {
		return
	}
	var marshalErr error
	if _, err := c.slot.Mutate(func(before []byte) []byte {
		merged := mergeEntries(before, snapshot)
		data, err := marshalBounded(merged)
		if err != nil {
			marshalErr = err
			return before // leave the slot untouched (the no-write idiom)
		}
		return data
	}); err != nil {
		c.logger.Warn("conditional cache save failed", "error", err)
		return
	}
	if marshalErr != nil {
		c.logger.Warn("conditional cache marshal failed", "error", marshalErr)
	}
}

// mergeEntries unions the persisted entries in before with snapshot, newer
// UsedAt winning per URL, dropping entries past retention. Self-healing per
// the SlotFile contract: torn or garbage bytes parse as an empty map.
func mergeEntries(before []byte, snapshot map[string]cacheEntry) map[string]cacheEntry {
	var persisted map[string]cacheEntry
	if len(before) > 0 {
		_ = json.Unmarshal(before, &persisted) // self-heal: garbage -> empty
	}
	merged := make(map[string]cacheEntry, len(persisted)+len(snapshot))
	now := time.Now()
	for url, e := range persisted {
		if now.Sub(e.UsedAt) <= condCacheRetention {
			merged[url] = e
		}
	}
	for url, e := range snapshot {
		if cur, ok := merged[url]; ok && cur.UsedAt.After(e.UsedAt) {
			continue
		}
		if now.Sub(e.UsedAt) <= condCacheRetention {
			merged[url] = e
		}
	}
	return merged
}

// marshalBounded marshals entries, evicting the least-recently-used entry
// until the payload fits condCacheMaxBytes, so the persisted copy never
// exceeds what the slot can read back. Eviction only trims the persisted
// copy; the in-memory map keeps everything for the process lifetime.
func marshalBounded(entries map[string]cacheEntry) ([]byte, error) {
	for {
		data, err := json.Marshal(entries)
		if err != nil {
			return nil, err
		}
		if len(data) <= condCacheMaxBytes || len(entries) == 0 {
			return data, nil
		}
		oldestURL := ""
		var oldest time.Time
		for url, e := range entries {
			if oldestURL == "" || e.UsedAt.Before(oldest) {
				oldestURL, oldest = url, e.UsedAt
			}
		}
		delete(entries, oldestURL)
	}
}
