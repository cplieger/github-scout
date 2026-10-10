package collect

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/runstate"
	"github.com/cplieger/slogx/capture"
)

// blockUntilDone holds the read named call until its context ends; fallback
// bounds the wait where no limit ever ends it.
func blockUntilDone(call string, fallback time.Duration) func(context.Context, string) error {
	return func(ctx context.Context, c string) error {
		if c != call {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(fallback):
			return nil
		}
	}
}

// wantTimeoutHint is the hint and reason a connection stopped at its limit
// logs.
const wantTimeoutHint = "This connection's scan ran past its limit, so its open work, workflows, alerts and scan complete were not logged. " +
	"The CI run lines it logged before the limit stand. Raise scan_interval, and raise lookback to match."

func TestScan_a_connection_past_its_limit_stops_and_the_others_publish(t *testing.T) {
	slow, fast := githubEndpoint("slow"), giteaEndpoint("fast")
	slow.conn.repos["o"] = append(slow.conn.repos["o"], forge.Repo{Path: "o/a", DefaultBranch: "main"})
	a := run(1, forge.RunFailing, now.Add(-time.Hour))
	a.Repo = "o/a"
	slow.conn.runs = map[string]listing{"o/a": {Runs: []forge.Run{a}}}
	slow.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x"}}}
	slow.conn.block = blockUntilDone("runs o/r", 5*time.Second)
	fast.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	fast.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 2, Author: "x"}}}
	// A 2s limit leaves a 200ms save reserve, far above a write under load.
	h := newLimitedHarness(t, 2*time.Second, slow, fast)
	if got := h.scan(t); got != Incomplete {
		t.Errorf("Scan with a connection past its limit = %s, want incomplete: that connection published nothing", got)
	}
	s := lineFor(t, h.rec, "scan stopped", "slow")
	if s["reason"] != "scan_timeout" || s["phase"] != "runs" || s["limit"] != "2s" || s["hint"] == "" || s["level"] != "WARN" {
		t.Errorf("scan stopped = %v, want a WARN scan_timeout in phase runs with limit 2s and a hint", s)
	}
	if l := lineFor(t, h.rec, "scan degraded", "slow"); l["cause"] != "scan_timeout" || l["reason"] != wantTimeoutHint {
		t.Errorf("scan degraded = %v, want cause scan_timeout with reason %q", l, wantTimeoutHint)
	}
	if s["hint"] != wantTimeoutHint {
		t.Errorf("scan stopped hint = %q, want %q: the ci run lines logged before the limit stand", s["hint"], wantTimeoutHint)
	}
	for _, msg := range []string{"scan complete", "open pull request", "failing workflow"} {
		noLineFor(t, h.rec, msg, "slow")
	}
	if l := lineFor(t, h.rec, "ci run", "slow"); l["repo"] != "o/a" {
		t.Errorf("slow ci run = %v, want the o/a run observed before the limit", l)
	}
	if n := h.c.store.Delivered("slow"); n != 1 {
		t.Errorf("Delivered(slow) = %d, want the 1 run observed before the limit kept", n)
	}
	c := lineFor(t, h.rec, "scan complete", "fast")
	if c["open_prs"] != "1" || c["degraded"] != "false" {
		t.Errorf("fast scan complete = %v, want its pull request and a clean scan", c)
	}
	lineFor(t, h.rec, "open pull request", "fast")
	lineFor(t, h.rec, "failing workflow", "fast")
	noLineFor(t, h.rec, "scan stopped", "fast")
	noLineFor(t, h.rec, "run state save failed", "slow")
	if l := lineFor(t, h.rec, "scan degraded", "slow"); strings.Contains(l["failed_signals"], "run_state") {
		t.Errorf("slow scan degraded failed_signals = %q, want no run_state: the save reserve leaves time to store the runs read before the limit", l["failed_signals"])
	}
	h.restart(t)
	if n := h.c.store.Delivered("slow"); n != 1 {
		t.Errorf("Delivered(slow) after a restart = %d, want 1: the save inside the reserve stored the run read before the limit", n)
	}
}

// stallCommit makes conn's save hang after its write until its context ends,
// falling back after fallback, as a write on a stalled file system does.
func stallCommit(t *testing.T, conn string, fallback time.Duration) {
	t.Helper()
	commitRuns = func(s *runstate.Store, ctx context.Context, name string, r *runstate.Read, deliver func([]runstate.Change) bool) error {
		err := s.Commit(ctx, name, r, deliver)
		if name != conn || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(fallback):
			return nil
		}
	}
	t.Cleanup(func() { commitRuns = (*runstate.Store).Commit })
}

func TestScan_duration_ms_includes_the_state_save(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	commitRuns = func(s *runstate.Store, ctx context.Context, name string, r *runstate.Read, deliver func([]runstate.Change) bool) error {
		err := s.Commit(ctx, name, r, deliver)
		*h.clock = h.clock.Add(80 * time.Second)
		return err
	}
	t.Cleanup(func() { commitRuns = (*runstate.Store).Commit })
	h.scan(t)
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["duration_ms"] != "80000" {
		t.Errorf("scan complete duration_ms with an 80s save = %q, want %q: the connection's scan includes its save", c["duration_ms"], "80000")
	}
}

func TestScan_a_save_past_the_limit_stops_its_connection_and_the_others_publish(t *testing.T) {
	slow, fast := githubEndpoint("slow"), giteaEndpoint("fast")
	slow.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	fast.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 2, Author: "x"}}}
	h := newLimitedHarness(t, 300*time.Millisecond, slow, fast)
	stallCommit(t, "slow", 5*time.Second)
	h.rec = h.resetRecorder(t)
	done := make(chan Outcome, 1)
	go func() { done <- h.c.Scan(t.Context()) }()
	select {
	case got := <-done:
		if got != Incomplete {
			t.Errorf("Scan with a save past the limit = %s, want incomplete", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Scan with a 300ms limit had not returned after 2s: the stalled save outlived the limit")
	}
	s := lineFor(t, h.rec, "scan stopped", "slow")
	if s["reason"] != "scan_timeout" || s["phase"] != "run state" {
		t.Errorf("scan stopped = %v, want scan_timeout in phase run state", s)
	}
	if l := lineFor(t, h.rec, "scan degraded", "slow"); !strings.Contains(l["failed_signals"], "run_state") {
		t.Errorf("slow scan degraded failed_signals = %q, want run_state named: its save was cut off at the limit", l["failed_signals"])
	}
	if l := lineFor(t, h.rec, "ci run", "slow"); l["run_id"] != "1" {
		t.Errorf("slow ci run = %v, want run 1 logged before the save stalled", l)
	}
	noLineFor(t, h.rec, "scan complete", "slow")
	if c := lineFor(t, h.rec, "scan complete", "fast"); c["open_prs"] != "1" || c["degraded"] != "false" {
		t.Errorf("fast scan complete = %v, want its pull request and a clean scan", c)
	}
}

// cancelAt cancels the scan when ep's GitHub-only client reads call.
func cancelAt(ep *endpoint, call string, cancel context.CancelFunc) {
	ep.gh.block = func(_ context.Context, c string) error {
		if c == call {
			cancel()
			return context.Canceled
		}
		return nil
	}
}

func TestScan_a_shutdown_mid_scan_still_saves_the_runs_it_read(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelAt(ep, "alerts o/r", cancel)
	h := newLimitedHarness(t, 2*time.Second, ep)
	h.rec = h.resetRecorder(t)
	if got := h.c.Scan(ctx); got != Interrupted {
		t.Fatalf("Scan cancelled after its runs read = %s, want interrupted", got)
	}
	if l := lineFor(t, h.rec, "ci run", "gh"); l["run_id"] != "1" {
		t.Errorf("ci run after a shutdown inside the save's grace = %v, want run 1 logged with the save that stores it", l)
	}
	h.restart(t)
	if n := h.c.store.Delivered("gh"); n != 1 {
		t.Errorf("Delivered(gh) after a restart = %d, want 1: the save after the shutdown stored run 1", n)
	}
}

func TestScan_a_run_line_the_save_grace_cuts_is_stored_nowhere_and_logged_after_a_restart(t *testing.T) {
	shutdownSaveGrace = 20 * time.Millisecond
	t.Cleanup(func() { shutdownSaveGrace = 5 * time.Second })
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{
		run(1, forge.RunFailing, now.Add(-2*time.Hour)), run(2, forge.RunPassing, now.Add(-time.Hour)),
	}}}
	h := newHarness(t, ep)
	var saveCtx context.Context
	commitRuns = func(s *runstate.Store, ctx context.Context, name string, r *runstate.Read, deliver func([]runstate.Change) bool) error {
		saveCtx = ctx
		return s.Commit(ctx, name, r, deliver)
	}
	t.Cleanup(func() { commitRuns = (*runstate.Store).Commit })
	h.rec = h.resetRecorder(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var once sync.Once
	graceEnded := true
	shutDuringFirstLine := func() {
		once.Do(func() {
			cancel()
			select {
			case <-saveCtx.Done():
			case <-time.After(2 * time.Second):
				graceEnded = false
			}
		})
	}
	h.c.logger = slog.New(onMsg{next: h.c.logger.Handler(), fn: shutDuringFirstLine, msg: "ci run"})
	if got := h.c.Scan(ctx); got != Interrupted {
		t.Errorf("Scan shut down during its first ci run line = %s, want interrupted", got)
	}
	if !graceEnded {
		t.Fatalf("Setup: the save's context had not ended 2s after the shutdown, want it ended by the 20ms grace")
	}
	if n := h.rec.CountExact("ci run"); n != 1 {
		t.Errorf("ci run lines with the grace ending during the first = %d, want 1: no line follows the cut", n)
	}
	h.restart(t)
	if n := h.c.store.Delivered("gh"); n != 0 {
		t.Errorf("Delivered(gh) after a restart = %d, want 0: run 2 was never logged, so no run of that read may be stored", n)
	}
	h.scan(t)
	if n := h.rec.CountExact("ci run"); n != 2 {
		t.Errorf("ci run lines of the scan after the restart = %d, want 2: both runs logged again, at least once", n)
	}
	if n := h.c.store.Delivered("gh"); n != 2 {
		t.Errorf("Delivered(gh) after the scan following the restart = %d, want 2", n)
	}
}

func TestScan_a_shutdown_waits_for_a_stalled_save_only_its_grace(t *testing.T) {
	shutdownSaveGrace = 100 * time.Millisecond
	t.Cleanup(func() { shutdownSaveGrace = 5 * time.Second })
	ep := githubEndpoint("gh")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelAt(ep, "alerts o/r", cancel)
	h := newHarness(t, ep)
	stallCommit(t, "gh", 5*time.Second)
	h.rec = h.resetRecorder(t)
	done := make(chan Outcome, 1)
	go func() { done <- h.c.Scan(ctx) }()
	select {
	case got := <-done:
		if got != Interrupted {
			t.Errorf("Scan cancelled with its save stalled = %s, want interrupted", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Scan cancelled with its save stalled had not returned after 2s, want it back within the 100ms grace")
	}
}

// onMsg passes every record to next, calling fn as each logged as msg
// arrives.
type onMsg struct {
	next slog.Handler
	fn   func()
	msg  string
}

func (h onMsg) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h onMsg) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.fn()
	}
	return h.next.Handle(ctx, r)
}

func (h onMsg) WithAttrs(a []slog.Attr) slog.Handler {
	return onMsg{next: h.next.WithAttrs(a), fn: h.fn, msg: h.msg}
}

func (h onMsg) WithGroup(name string) slog.Handler {
	return onMsg{next: h.next.WithGroup(name), fn: h.fn, msg: h.msg}
}

// burstMsgs are the lines a scan's burst logs after its last read.
var burstMsgs = map[string]bool{
	"open pull request": true, "open issue": true, "failing workflow": true, "slow workflow": true,
	"disabled workflow": true, "security alert": true, "top security alert": true, "security tool": true,
	"scan stopped": true, "scan degraded": true, "scan complete": true,
}

// cutAt passes every record to next and calls cancel as the burst line
// numbered at, counted from 0 in n, arrives.
type cutAt struct {
	next   slog.Handler
	n      *atomic.Int64
	cancel func()
	at     int64
}

func (h cutAt) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h cutAt) Handle(ctx context.Context, r slog.Record) error {
	if burstMsgs[r.Message] && h.n.Add(1)-1 == h.at {
		h.cancel()
	}
	return h.next.Handle(ctx, r)
}

func (h cutAt) WithAttrs(a []slog.Attr) slog.Handler {
	h.next = h.next.WithAttrs(a)
	return h
}

func (h cutAt) WithGroup(name string) slog.Handler {
	h.next = h.next.WithGroup(name)
	return h
}

// fullBurstHarness scans three connections whose burst logs every kind of
// burst line: gh reads every signal whole, gt completes degraded, and st,
// configured last, stops at a rate limit and degrades.
func fullBurstHarness(t *testing.T) *harness {
	t.Helper()
	gh, gt, st := githubEndpoint("gh"), giteaEndpoint("gt"), githubEndpoint("st")
	gh.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x"}}}
	gh.conn.issues = map[string][]forge.Issue{"o": {{Repo: "o/r", Number: 2, Author: "x"}}}
	var runs []forge.Run
	for i := range int64(3) {
		runs = append(runs, run(i+1, forge.RunFailing, now.Add(-time.Duration(i+1)*time.Hour)))
	}
	gh.conn.runs = map[string]listing{"o/r": {Runs: runs}}
	gh.gh.workflows = map[string][]forge.Workflow{"o/r": {
		{Repo: "o/r", Name: "CI", State: "active"}, {Repo: "o/r", Name: "nightly", State: "disabled_manually"},
	}}
	gh.gh.alerts = map[string][]forge.Alert{"o/r": {{Repo: "o/r", Number: 3, Rule: "rule", Severity: "high", Tool: "CodeQL"}}}
	gt.conn.issues = map[string][]forge.Issue{"o": {{Repo: "o/r", Number: 4, Author: "x"}}}
	gt.conn.errs = map[string]error{"prs o": errors.New("listing failed")}
	st.conn.errs = map[string]error{"runs o/r": forge.ErrRateLimited}
	return newHarness(t, gh, gt, st)
}

// burstLines is each burst line rec holds, as message|connection, and
// every message rec holds after the burst line numbered cut, counted from 0;
// a negative cut collects none.
func burstLines(rec *capture.Recorder, cut int) (burst, after []string) {
	for _, r := range rec.Records() {
		if len(burst) > cut && cut >= 0 {
			after = append(after, r.Message)
		}
		if !burstMsgs[r.Message] {
			continue
		}
		conn := ""
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "connection" {
				conn = a.Value.String()
			}
			return true
		})
		burst = append(burst, r.Message+"|"+conn)
	}
	return burst, after
}

func TestScan_a_shutdown_at_any_burst_line_logs_no_line_after_it(t *testing.T) {
	h := fullBurstHarness(t)
	if got := h.scan(t); got != Incomplete {
		t.Fatalf("Setup: uncut scan = %s, want incomplete: st stops", got)
	}
	want, _ := burstLines(h.rec, -1)
	kinds := map[string]bool{}
	for _, l := range want {
		msg, _, _ := strings.Cut(l, "|")
		kinds[msg] = true
	}
	if len(kinds) != len(burstMsgs) || !slices.Contains(want, "scan degraded|st") {
		t.Fatalf("Setup: uncut burst %v, want every burst line kind and st's scan degraded", want)
	}
	for at := range want {
		t.Run(strconv.Itoa(at), func(t *testing.T) {
			h := fullBurstHarness(t)
			h.rec = h.resetRecorder(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h.c.logger = slog.New(cutAt{next: h.c.logger.Handler(), n: new(atomic.Int64), cancel: cancel, at: int64(at)})
			if got := h.c.Scan(ctx); got != Interrupted {
				t.Errorf("Scan cut at burst line %d (%s) = %s, want interrupted", at, want[at], got)
			}
			got, after := burstLines(h.rec, at)
			if !slices.Equal(got, want[:at+1]) {
				t.Errorf("Scan cut at burst line %d (%s) logged burst %v, want the uncut burst up to the cut %v", at, want[at], got, want[:at+1])
			}
			if !slices.Equal(after, []string{"scan interrupted; no further line emitted"}) {
				t.Errorf("Scan cut at burst line %d (%s) logged %v after it, want only the interruption", at, want[at], after)
			}
		})
	}
}

func TestScan_a_limit_reached_in_a_read_names_that_read_as_its_phase(t *testing.T) {
	for call, phase := range map[string]string{"alerts o/r": "security", "workflows o/r": "workflows"} {
		t.Run(phase, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.gh.block = blockUntilDone(call, 3*time.Second)
			h := newLimitedHarness(t, 200*time.Millisecond, ep)
			h.scan(t)
			if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "scan_timeout" || s["phase"] != phase {
				t.Errorf("scan stopped with the limit reached in %q = %v, want scan_timeout in phase %s", call, s, phase)
			}
		})
	}
	ep := githubEndpoint("gh")
	ep.conn.block = blockUntilDone("issues o", 3*time.Second)
	h := newLimitedHarness(t, 200*time.Millisecond, ep)
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["phase"] != "open issues" {
		t.Errorf("scan stopped with the limit reached listing issues = %v, want phase open issues", s)
	}
}

func TestScan_reads_every_connection_at_the_same_time(t *testing.T) {
	a, b := githubEndpoint("a"), giteaEndpoint("b")
	bStarted := make(chan struct{})
	a.conn.block = func(ctx context.Context, call string) error {
		if call != "discover o" {
			return nil
		}
		select {
		case <-bStarted:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			return errors.New("b was never read while a waited")
		}
	}
	var once sync.Once
	b.conn.block = func(_ context.Context, call string) error {
		if call == "discover o" {
			once.Do(func() { close(bStarted) })
		}
		return nil
	}
	h := newLimitedHarness(t, 2*time.Second, a, b)
	if got := h.scan(t); got != Complete {
		t.Errorf("Scan = %s, want complete: both connections read; messages %v", got, h.rec.Messages())
	}
	lineFor(t, h.rec, "scan complete", "a")
	lineFor(t, h.rec, "scan complete", "b")
}

func TestScan_only_budget_stops_share_a_budget_windows_log_level(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.block = blockUntilDone("runs o/r", 3*time.Second)
	h := newLimitedHarness(t, 200*time.Millisecond, ep)
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "scan_timeout" {
		t.Fatalf("scan 1 stopped = %v, want scan_timeout", s)
	}
	ep.conn.block = nil
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "read_reserve" || s["level"] != "WARN" {
		t.Errorf("first read_reserve stop of the budget window after a timeout = %v, want level WARN", s)
	}
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["level"] != "INFO" {
		t.Errorf("second read_reserve stop of the same budget window = %v, want level INFO", s)
	}
	ep.conn.errs = nil
	ep.conn.block = blockUntilDone("runs o/r", 3*time.Second)
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "scan_timeout" || s["level"] != "WARN" {
		t.Errorf("timeout stop after budget stops of the same window = %v, want level WARN", s)
	}
}

func TestScan_a_budget_stop_a_shutdown_withheld_leaves_the_next_one_WARN(t *testing.T) {
	gh, st := githubEndpoint("gh"), githubEndpoint("st")
	gh.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x"}}}
	st.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
	h := newHarness(t, gh, st)
	h.rec = h.resetRecorder(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.c.logger = slog.New(onMsg{next: h.c.logger.Handler(), fn: cancel, msg: "open pull request"})
	if got := h.c.Scan(ctx); got != Interrupted {
		t.Fatalf("Setup: Scan cut at its first burst line = %s, want interrupted", got)
	}
	noLineFor(t, h.rec, "scan stopped", "st")
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "st"); s["level"] != "WARN" {
		t.Errorf("first logged read_reserve stop of the budget window = %v, want level WARN: the cut scan logged none", s)
	}
}

func TestScan_every_budget_stop_stays_visible_at_the_default_log_level(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
	h := newHarness(t, ep)
	var out bytes.Buffer
	h.c.logger = slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for range 3 {
		h.c.Scan(t.Context())
	}
	if n := strings.Count(out.String(), `"msg":"scan stopped"`); n != 3 {
		t.Errorf("three scans stopped at the reserve in one budget window logged %d scan stopped lines at info, want 3: "+
			"each is the stall alert's heartbeat; output %s", n, out.String())
	}
}

// TestScan_a_panic_in_a_connection_ends_the_process runs a panicking scan in
// a child test process and checks the panic ends it. The parent owns the
// state directory: the child dies before its own cleanup could run.
func TestScan_a_panic_in_a_connection_ends_the_process(t *testing.T) {
	if dir := os.Getenv("FORGE_SCOUT_PANIC_STATE"); dir != "" {
		c := New(t.Context(), &Deps{
			Open: func(context.Context, *config.Connection) (Conn, GitHubReader, error) {
				panic("simulated scan panic")
			},
			Store:       openStore(t, dir),
			Connections: []config.Connection{{Name: "a"}},
		})
		c.Scan(t.Context())
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestScan_a_panic_in_a_connection_ends_the_process$")
	cmd.Env = append(os.Environ(), "FORGE_SCOUT_PANIC_STATE="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(string(out), "simulated scan panic") {
		t.Errorf("child scan with a panicking connection = %v, output %q, want the panic to end the process", err, out)
	}
}
