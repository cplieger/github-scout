package runstate

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
)

// modelRun is one run of the model forge; state is empty while it runs.
type modelRun struct {
	created, updated time.Time
	wf               string
	state            forge.RunState
	id               int64
}

func (m *modelRun) newer(o *modelRun) bool {
	return cmp.Or(m.created.Compare(o.created), cmp.Compare(m.id, o.id)) > 0
}

type reading struct {
	updated time.Time
	state   forge.RunState
	id      int64
}

// model is a forge's runs and what the store's listings of them returned.
type model struct {
	rng    *rand.Rand
	now    time.Time
	listed map[int64]modelRun
	runs   []*modelRun
	// smooth is false once the store had no listing time to follow or
	// reported a gap past the window: from then on a run may have finished
	// unlisted with the store knowing.
	smooth bool
	// pending is the runs the last listing returned still running, and
	// belowCut those of them a later listing does not reach: a run that may
	// finish with no listing reaching it again, which the store reports only
	// below a cut (Stranded).
	pending, belowCut map[int64]bool
}

func (m *model) state() forge.RunState {
	return []forge.RunState{forge.RunPassing, forge.RunFailing}[m.rng.IntN(2)]
}

// step advances the forge by gap: new runs, runs finishing, re-runs of
// recent ones.
func (m *model) step(gap time.Duration, nextID *int64) {
	prev := m.now
	m.now = m.now.Add(gap)
	for _, r := range m.runs {
		switch {
		case r.state == "":
			r.state, r.updated = m.state(), m.now.Add(-time.Minute)
		case m.now.Sub(r.created) < 40*time.Hour && m.rng.IntN(8) == 0:
			r.state, r.updated = m.state(), m.now.Add(-30*time.Second)
		}
	}
	for range m.rng.IntN(5) {
		created := prev.Add(time.Duration(m.rng.Int64N(int64(gap)))).Truncate(time.Second)
		r := &modelRun{created: created, updated: created.Add(time.Minute), wf: []string{"A", "B", "C"}[m.rng.IntN(3)], id: *nextID}
		*nextID++
		if m.rng.IntN(4) != 0 {
			r.state = m.state()
		}
		m.runs = append(m.runs, r)
	}
}

// listing is the scan's listing of the window, newest first, cut short at a
// random row when cut.
func (m *model) listing(cut bool) (Listing, []*modelRun) {
	window := m.now.Add(-lookback)
	var rows []*modelRun
	for _, r := range m.runs {
		if !r.created.Before(window) {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b *modelRun) int { return cmp.Or(b.created.Compare(a.created), cmp.Compare(b.id, a.id)) })
	l := Listing{Repo: "o/r", Cover: window, Cut: cut}
	if cut {
		rows = rows[:m.rng.IntN(len(rows)+1)]
		l.Cover = m.now
		if len(rows) > 0 {
			l.Cover = rows[len(rows)-1].created
		}
	}
	for _, r := range rows {
		if r.state == "" {
			if l.Pending.IsZero() || r.created.Before(l.Pending) {
				l.Pending = r.created
			}
			continue
		}
		l.Runs = append(l.Runs, Observation{Run: forge.Run{
			CreatedAt: r.created, StartedAt: r.created, UpdatedAt: r.updated, Repo: "o/r", Workflow: r.wf,
			Branch: "main", Trigger: "push", State: r.state, ID: r.id,
		}})
		m.listed[r.id] = *r
	}
	return l, rows
}

// track marks the runs the previous listing returned still running that l
// does not reach, then records which of rows still run. It reports whether l,
// cut short, left one of them below it.
func (m *model) track(l *Listing, rows []*modelRun) (stranded bool) {
	for _, r := range m.runs {
		if m.pending[r.id] && r.created.Before(l.Cover) {
			m.belowCut[r.id] = true
			stranded = stranded || l.Cut
		}
	}
	clear(m.pending)
	for _, r := range rows {
		if r.state == "" {
			m.pending[r.id] = true
		}
	}
	return stranded
}

// newest is each workflow's newest run of runs within the retention that
// pick accepts.
func (m *model) newest(runs []modelRun, pick func(*modelRun) bool) map[string]modelRun {
	out := map[string]modelRun{}
	for i := range runs {
		r := &runs[i]
		if r.created.Before(m.now.Add(-WorkflowRetention)) || !pick(r) {
			continue
		}
		if n, ok := out[r.wf]; !ok || r.newer(&n) {
			out[r.wf] = *r
		}
	}
	return out
}

func (m *model) real() []modelRun {
	out := make([]modelRun, 0, len(m.runs))
	for _, r := range m.runs {
		out = append(out, *r)
	}
	return out
}

// TestCommit_matches_a_model_of_every_run_its_listings_returned drives the
// store through random scans with re-runs, pending runs, listings cut short,
// gaps in and past the window and restarts. Every workflow's verdict must be
// the newest run any listing returned, in its last listed state; a verdict a
// cut listing vouches for, and every verdict while the store has reported no
// gap, must be the forge's own newest finished run, bar one a later listing
// left below it while it ran (which a cut listing must report as Stranded);
// and every listed reading is delivered exactly once.
func TestCommit_matches_a_model_of_every_run_its_listings_returned(t *testing.T) {
	for seed := range uint64(120) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { modelScans(t, seed) })
	}
}

func modelScans(t *testing.T, seed uint64) {
	m := &model{
		rng: rand.New(rand.NewPCG(seed, 7)), now: t0, listed: map[int64]modelRun{}, smooth: true,
		pending: map[int64]bool{}, belowCut: map[int64]bool{},
	}
	s := open(t, t.TempDir())
	nextID := int64(1)
	readings, delivered := map[reading]bool{}, map[reading]int{}
	for step := range 60 {
		// Two in-window gaps can sum past the window, so a listing cut short
		// between them can leave a span the next listing never reaches.
		gap := time.Duration(15+m.rng.IntN(50*60)) * time.Minute
		if m.rng.IntN(12) == 0 {
			gap = time.Duration(80+m.rng.IntN(200)) * time.Hour
		}
		m.step(gap, &nextID)
		cut := m.rng.IntN(3) == 0
		l, rows := m.listing(cut)
		if m.track(&l, rows) && !Stranded(l.Cut, l.Cover, s.Pending("gh", "o/r")) {
			t.Fatalf("seed %d step %d: a cut listing from %s left a run the last listing returned pending below it, Pending = %s, want Stranded", seed, step, l.Cover, s.Pending("gh", "o/r"))
		}
		window := m.now.Add(-lookback)
		if prev := s.LastListed("gh", "o/r"); step > 0 && (prev.IsZero() || !Covered(prev, window)) {
			m.smooth = false
		}
		for _, o := range l.Runs {
			readings[reading{id: o.Run.ID, state: o.Run.State, updated: o.Run.UpdatedAt}] = true
		}
		read := &Read{Start: m.now, Window: window, Listings: []Listing{l}}
		if err := s.Commit(t.Context(), "gh", read, func(chs []Change) bool {
			for _, ch := range chs {
				delivered[reading{id: ch.Run.ID, state: ch.Run.State, updated: ch.Run.UpdatedAt}]++
			}
			return true
		}); err != nil {
			t.Fatalf("seed %d step %d: Commit = %v", seed, step, err)
		}
		if m.rng.IntN(6) == 0 {
			s = reload(t, s)
		}
		m.check(t, s, &l, seed, step)
	}
	for k := range readings {
		if n := delivered[k]; n != 1 {
			t.Errorf("seed %d: reading %+v delivered %d time(s), want exactly 1 with every write landing", seed, k, n)
		}
	}
}

func (m *model) check(t *testing.T, s *Store, l *Listing, seed uint64, step int) {
	t.Helper()
	listed := make([]modelRun, 0, len(m.listed))
	for _, r := range m.listed {
		listed = append(listed, r)
	}
	done := func(r *modelRun) bool { return r.state != "" }
	oracle, real := m.newest(listed, done), m.newest(m.real(), done)
	got := map[string]WorkflowState{}
	for k, w := range s.Workflows("gh") {
		got[k.Name] = w
	}
	for wf, n := range oracle {
		if w, ok := got[wf]; !ok || w.RunID != n.id || w.State != n.state {
			t.Fatalf("seed %d step %d: workflow %s verdict %+v (held %t), want run %d %s, the newest any listing returned", seed, step, wf, w, ok, n.id, n.state)
		}
	}
	for wf, w := range got {
		if _, ok := oracle[wf]; !ok {
			t.Fatalf("seed %d step %d: workflow %s verdict %+v, want none: no listing returned a run of it in the retention", seed, step, wf, w)
		}
		if !Vouched(l.Cut, l.Cover, w.Last) {
			continue
		}
		r := real[wf]
		if m.belowCut[r.id] && m.listed[r.id].state != r.state {
			continue
		}
		if (l.Cut || m.smooth) && (w.RunID != r.id || w.State != r.state) {
			t.Fatalf("seed %d step %d: workflow %s verdict %+v with no gap reported, want run %d %s, the forge's newest finished", seed, step, wf, w, r.id, r.state)
		}
	}
}
