// Package collect runs forge-scout's scans: for each configured connection it
// reads repositories, open pull requests and issues, CI runs and, on GitHub,
// code-scanning alerts and disabled workflows, and emits them as structured
// log lines. CI runs are events, logged when a run is new or its state or
// update time changes; everything else is a snapshot, emitted in one burst
// after the scan's last read and tied together by scan_id and rank. Each
// connection's integrity is judged and reported on its own, so one dead forge
// never blanks another's signals.
package collect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/runstate"
)

// Conn is one open forge connection, as the collector reads it.
type Conn interface {
	Product() forge.Product
	Login() string
	Budget() forge.Budget
	Close()
	Discover(ctx context.Context, owners []string) (forge.Discovery, error)
	OpenPRs(ctx context.Context, owner string) ([]forge.PullRequest, error)
	OpenIssues(ctx context.Context, owner string) ([]forge.Issue, error)
	RepoOpenPRs(ctx context.Context, repo forge.Repo) ([]forge.PullRequest, error)
	RepoOpenIssues(ctx context.Context, repo forge.Repo) ([]forge.Issue, error)
	RunsSince(ctx context.Context, repo forge.Repo, since time.Time) (forge.RunList, error)
	CommitChecks(ctx context.Context, pr *forge.PullRequest) (forge.CheckResult, error)
}

// GitHubReader reads the GitHub-only signals. It and the connection share
// one quota meter, which admits every request either sends (see
// ghquota.Meter): BeginScan starts a scan on it, and Budget is the account's
// shared budget.
type GitHubReader interface {
	CodeScanningAlerts(ctx context.Context, repo forge.Repo) (forge.Listing[forge.Alert], error)
	Workflows(ctx context.Context, repo forge.Repo) (forge.Listing[forge.Workflow], error)
	BeginScan()
	Budget() forge.Budget
}

// Opener opens a connection. It returns a GitHubReader for a GitHub product
// and nil for any other.
type Opener func(ctx context.Context, c *config.Connection) (Conn, GitHubReader, error)

// Deps are the collector's collaborators. Store is required, and the caller
// closes it. Logger carries neither forge nor connection: the collector
// scopes every line it logs (see Scope). A nil Logger is slog.Default; a nil
// Now is time.Now.
type Deps struct {
	Open        Opener
	Store       *runstate.Store
	Logger      *slog.Logger
	Now         func() time.Time
	Connections []config.Connection
	Lookback    time.Duration
	// ScanInterval is the configured gap between scans, which scan complete
	// and scan stopped report so a reader can tell a stalled loop.
	ScanInterval time.Duration
	// ScanLimit bounds each connection's scan, its save included; 0 is no
	// limit.
	ScanLimit time.Duration
}

// Collector holds the open connections and the run state between scans.
// It is not safe for concurrent use.
type Collector struct {
	open      Opener
	store     *runstate.Store
	logger    *slog.Logger
	now       func() time.Time
	lastStops map[string]time.Time
	conns     []*connState
	lookback  time.Duration
	interval  time.Duration
	limit     time.Duration
}

type connState struct {
	conn Conn
	gh   GitHubReader
	// unmapped is the runs in a state forgeapi could not map that the
	// connection's last scan listed, by repository path and ID.
	unmapped map[string]bool
	// lastEmpty is the owners_empty value of the connection's last
	// discovery.
	lastEmpty string
	cfg       config.Connection
	// idx is the connection's place in the configuration.
	idx int
}

// New reclaims abandoned state temp files and loads the run state: a missing
// file is a silent cold start, an unusable one a cold start it logs.
func New(ctx context.Context, d *Deps) *Collector {
	c := &Collector{
		open: d.Open, store: d.Store, logger: d.Logger, now: d.Now, lookback: d.Lookback, interval: d.ScanInterval, limit: d.ScanLimit,
		lastStops: map[string]time.Time{},
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	for i := range d.Connections {
		c.conns = append(c.conns, &connState{cfg: d.Connections[i], idx: i})
	}
	if failed, err := c.store.CleanupTemps(ctx); err != nil || failed > 0 {
		c.process().Warn("run state temp files not reclaimed", "failed", failed, "error", err)
	}
	discarded, err := c.store.Load(ctx)
	switch {
	case discarded != nil && err == nil:
		c.process().Warn("run state unreadable; starting cold", "path", c.store.Path(), "set_aside", discarded.Aside, "error", discarded.Reason)
	case discarded != nil:
		c.process().Error("run state unreadable; starting cold", "path", c.store.Path(), "error", discarded.Reason, "set_aside_error", err)
	case err != nil:
		c.process().Warn("run state not loaded; starting cold", "path", c.store.Path(), "error", err)
	}
	return c
}

// Scope returns base with the forge and connection every forge-scout line
// carries. A line about no single connection carries an empty connection, and
// forge unknown unless it is about one product's connections together.
func Scope(base *slog.Logger, product forge.Product, connection string) *slog.Logger {
	return base.With("forge", product.String(), "connection", connection)
}

func (c *Collector) process() *slog.Logger { return Scope(c.logger, forge.ProductUnknown, "") }

// Close closes every open connection.
func (c *Collector) Close() {
	for _, cs := range c.conns {
		if cs.conn != nil {
			cs.conn.Close()
			cs.conn, cs.gh = nil, nil
		}
	}
}

// Outcome is what one scan established.
type Outcome int

// The Outcome members. The zero value is Incomplete, so an outcome nobody
// set never reads as Complete.
const (
	// Incomplete is a scan in which some connection stopped, or did not read
	// every signal its forge has whole.
	Incomplete Outcome = iota
	// Interrupted is a scan whose context ended before it returned; it
	// logged no line of its burst after that.
	Interrupted
	// Complete is a scan in which every connection read every signal its
	// forge has whole.
	Complete
)

func (o Outcome) String() string { return [...]string{"incomplete", "interrupted", "complete"}[o] }

var _ fmt.Stringer = Complete

// Scan runs one scan of every connection, each in its own goroutine under its
// own limit, which its save shares (see saveReserve). Each connection logs
// each run change just before the write that stores it, so a crash between
// the two repeats the line after a restart and never drops one. A cancelled
// context ends the scan Interrupted and its burst at the line it reached (see
// emitter); runs are still logged and saved while shutdownSaveGrace lasts,
// and a run whose line the grace cut is stored nowhere, so the next start
// logs it.
func (c *Collector) Scan(ctx context.Context) Outcome {
	start := c.now()
	sc := &scan{
		id: start.UnixMilli(), start: start, cutoff: start.Add(-c.lookback).Truncate(time.Second), state: c.store,
		gates: c.accountGates(), out: emitter{done: ctx.Done()},
	}
	results := make([]*connResult, len(c.conns))
	var wg sync.WaitGroup
	for i, cs := range c.conns {
		wg.Go(func() {
			began := c.now()
			var saveBy time.Time
			if c.limit > 0 {
				saveBy = time.Now().Add(c.limit)
			}
			rctx, cancel := c.reading(ctx, saveBy)
			r := c.scanConn(rctx, sc, cs)
			cancel()
			pctx, release := saving(ctx, saveBy)
			c.save(pctx, sc, r)
			release()
			r.sum.duration = c.now().Sub(began)
			results[i] = r
		})
	}
	wg.Wait()
	for _, r := range results {
		if r.ledger.stop == nil {
			c.derive(sc, r)
		}
	}
	c.emitBurst(sc, results)
	if sc.out.interrupted() {
		c.process().Debug("scan interrupted; no further line emitted")
		return Interrupted
	}
	for _, r := range results {
		if !r.ledger.complete() {
			return Incomplete
		}
	}
	return Complete
}

// accountGate holds the connections to one forge until each has opened, so
// that of two on one account the earlier configured is the one that reads,
// whichever opened first.
type accountGate struct {
	ready  chan struct{}
	logins map[int]string
	left   int
	mu     sync.Mutex
}

// accountGates is each connection's gate, shared by the connections whose
// url names the same forge.
func (c *Collector) accountGates() []*accountGate {
	byURL := map[string]*accountGate{}
	out := make([]*accountGate, len(c.conns))
	for i, cs := range c.conns {
		key := cs.cfg.Instance()
		g := byURL[key]
		if g == nil {
			g = &accountGate{ready: make(chan struct{}), logins: map[int]string{}}
			byURL[key] = g
		}
		g.left++
		out[i] = g
	}
	return out
}

// arrive records connection i as open on login, or not open when login is
// empty. Every connection of the gate arrives once per scan.
func (g *accountGate) arrive(i int, login string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.logins[i] = login
	g.left--
	if g.left == 0 {
		close(g.ready)
	}
}

// first waits for every connection of the gate and returns the earliest
// configured before i that opened on login, -1 for none.
func (g *accountGate) first(ctx context.Context, i int, login string) (int, error) {
	select {
	case <-g.ready:
	case <-ctx.Done():
		return -1, context.Cause(ctx)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for j := range i {
		if l, ok := g.logins[j]; ok && l != "" && strings.EqualFold(l, login) {
			return j, nil
		}
	}
	return -1, nil
}

// errScanTimeout is the cause of a connection's context ending at its limit.
var errScanTimeout = errors.New("connection scan past its limit")

// saveReserve is the part of a connection's limit its reads leave to its
// save, so a connection that reads up to its limit still stores its runs.
func saveReserve(limit time.Duration) time.Duration { return min(limit/10, time.Minute) }

// reading is the context of a connection's reads: ctx, ended saveReserve
// before saveBy, or never by time when saveBy is zero.
func (c *Collector) reading(ctx context.Context, saveBy time.Time) (context.Context, context.CancelFunc) {
	if saveBy.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadlineCause(ctx, saveBy.Add(-saveReserve(c.limit)), errScanTimeout)
}

// shutdownSaveGrace is how long a save runs on once the scan's context ends,
// half of Docker's default 10s stop timeout
// (https://docs.docker.com/reference/cli/docker/container/stop/); a var so a
// test can lower it.
var shutdownSaveGrace = 5 * time.Second

// saving is the context of a connection's save: it ends at saveBy, with
// cause errScanTimeout, or shutdownSaveGrace after ctx ends, whichever is
// first, so a shutdown mid-scan still stores what was read.
func saving(ctx context.Context, saveBy time.Time) (sctx context.Context, release func()) {
	bounded := context.WithoutCancel(ctx)
	endBound := context.CancelFunc(func() {})
	if !saveBy.IsZero() {
		bounded, endBound = context.WithDeadlineCause(bounded, saveBy, errScanTimeout)
	}
	sctx, cancel := context.WithCancelCause(bounded)
	grace := shutdownSaveGrace
	stop := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel(context.Cause(ctx))
		case <-sctx.Done():
		}
	})
	return sctx, func() {
		stop()
		cancel(nil)
		endBound()
	}
}

// commitRuns is the store's Commit; a var so a test can stand in for the
// filesystem's answer.
var commitRuns = (*runstate.Store).Commit

// save commits what the connection read, logging its run changes before the
// write (see Scan) through an emitter on the save's context: a line the
// save's end refuses leaves the read unrecorded, never stored unlogged. A
// failed or not durable write marks the connection's scan degraded, and one
// cut off at the limit stops it; the state in memory keeps every read it
// recorded, for a later write, and a save cut off behind an earlier write
// recorded nothing, which the next scan reads again.
func (c *Collector) save(ctx context.Context, sc *scan, r *connResult) {
	defined := make(map[string][]string, len(r.defined))
	for _, d := range r.defined {
		defined[d.path] = slices.Concat(slices.Collect(maps.Keys(d.live)), slices.Collect(maps.Keys(d.deleted)))
	}
	read := &runstate.Read{Start: sc.start, Window: sc.cutoff, Kept: r.keptWhole, Defined: defined, Listings: r.listings()}
	r.ledger.begin(famState)
	defer r.ledger.finish(famState)
	out := emitter{done: ctx.Done()}
	err := commitRuns(c.store, ctx, r.cfg.Name, read, func(changes []runstate.Change) bool { return r.emitRuns(out, changes) })
	switch {
	case errors.Is(err, runstate.ErrNotDurable):
		r.ledger.drop(famState, dropNotDurable, 1)
		r.log.Error("run state not durable", "path", c.store.Path(), "error", err)
	case err != nil:
		r.ledger.drop(famState, dropUnwritable, 1)
		r.log.Error("run state save failed", "path", c.store.Path(), "error", err)
		if r.ledger.stop == nil && errors.Is(context.Cause(ctx), errScanTimeout) {
			r.ledger.timedOut = true
			r.ledger.halt(stopScanTimeout, phaseState)
		}
	}
}

// ensureOpen opens the connection when it is not open yet, and reports
// whether this call opened it.
func (c *Collector) ensureOpen(ctx context.Context, cs *connState) (opened bool, err error) {
	if cs.conn != nil {
		return false, nil
	}
	conn, gh, err := c.open(ctx, &cs.cfg)
	if err != nil {
		return false, err
	}
	cs.conn, cs.gh = conn, gh
	return true, nil
}

// The scan stopped reasons.
const (
	stopReadReserve = "read_reserve"
	stopRateLimited = "rate_limited"
	stopScanTimeout = "scan_timeout"
)

// stopErr reports a read that ends the connection's scan: the read reserve
// reached, or the forge refusing further requests.
func stopErr(err error) (reason string, stop bool) {
	switch {
	case errors.Is(err, forge.ErrReadDeferred):
		return stopReadReserve, true
	case errors.Is(err, forge.ErrRateLimited):
		return stopRateLimited, true
	}
	return "", false
}
