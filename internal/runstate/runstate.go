// Package runstate persists, per connection, what forge-scout has judged of
// the CI runs it listed: each workflow's verdict on each branch, from its
// newest judged run, the last state delivered of each run still inside the
// lookback window, so a container start does not re-emit the window, and
// the runs created on each of the last dayRetention UTC days in their newest
// state. A workflow whose last run fell out of the lookback stays known from
// its verdict. Every scan lists the whole window again; the store never says
// where a listing starts.
//
// One process owns a store directory: Open locks it for the life of the
// Store, the state lives in memory, and each Commit replaces the whole file
// atomically, delivering its changes first, so each is delivered at least
// once: one whose write never landed is delivered again after a restart.
package runstate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v4"
	"github.com/cplieger/github-scout/internal/forge"
)

const fileName = "state.json"

// lockName is the file Open locks. It is not the data file because the
// atomic rename swaps the data file's inode under any lock held on it.
const lockName = "state.lock"

// WorkflowRetention is how long a workflow's branch verdict with no newer run
// stays known: a monthly workflow survives one missed month, whose longest
// two-month span is 62 days.
const WorkflowRetention = 63 * 24 * time.Hour

// dayRetention is how many UTC days, the scan's own included, the store keeps
// each day's runs for, whatever the lookback: a week of day charts and the
// day it ends on.
const dayRetention = 8

const formatVersion = 2

// maxStateBytes bounds the file read and write; a var so a test can lower it.
// A delivered run takes under 150 bytes, so a deployment seen holding 8,115
// runs over a 72h lookback holds about 81,000 (12 MiB) at the 720h maximum,
// plus about 22,000 day records of 50 bytes, and 128 MiB is ten times that.
var maxStateBytes int64 = 128 << 20

// ErrInUse is Open's error when another process holds the directory.
var ErrInUse = errors.New("state directory is in use by another forge-scout process")

// ErrNotDurable is Commit's error when the state file was replaced but its
// directory could not be synced, so a power loss could still undo the write.
var ErrNotDurable = errors.New("run state written but not made durable")

// errClosed is Commit's error once Close has been called.
var errClosed = errors.New("run state store is closed")

// errUndelivered is Commit's error when deliver did not deliver every change.
var errUndelivered = errors.New("run changes not delivered; read not recorded")

// writeState writes the state file; a var so a test can stand in for the
// filesystem's answer.
var writeState = atomicfile.WriteFile

// readState reads the state file; a var so a test can watch the read.
var readState = atomicfile.ReadBoundedInRoot

// Store is the run state of one directory, held by this process. Its methods
// are safe for concurrent use.
type Store struct {
	lock *os.File
	st   *state
	// writing holds the right to write the file; mu guards st, closed and
	// lock, and is never held across a write.
	writing chan struct{}
	dir     string
	mu      sync.Mutex
	closed  bool
}

// Open locks dir for this process and returns its store, empty until Load.
// It answers ErrInUse while another Store, in this process or another, holds
// dir. Call Close to release it.
func Open(dir string) (*Store, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open run state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("lock run state: %w", err)
	}
	return &Store{dir: dir, lock: f, st: newState(), writing: make(chan struct{}, 1)}, nil
}

// Close releases the directory once no write a Commit started can still
// replace the file, so no successor's state is overwritten by one; a write
// that never ends holds it until the process exits. A Commit after Close
// delivers nothing and fails.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	select {
	case s.writing <- struct{}{}:
		s.unlock()
		<-s.writing
	default:
		// The running write's endWrite releases it.
	}
}

// takeWrite takes the right to write, or reports false when ctx ends while
// a running write holds it. A free right is taken even once ctx has ended,
// so a Commit still records and delivers its read.
func (s *Store) takeWrite(ctx context.Context) bool {
	select {
	case s.writing <- struct{}{}:
		return true
	default:
	}
	select {
	case s.writing <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// endWrite gives up the right to write, releasing the directory when the
// Store was closed meanwhile.
func (s *Store) endWrite() {
	s.mu.Lock()
	defer s.mu.Unlock()
	<-s.writing
	if s.closed {
		s.unlock()
	}
}

// unlock releases the directory lock; s.mu must be held.
func (s *Store) unlock() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}

// Path is the state file's path.
func (s *Store) Path() string { return filepath.Join(s.dir, fileName) }

// Discarded is a state file Load could not use: Reason says why, and Aside
// is where it was moved, empty when the move failed.
type Discarded struct {
	Reason error
	Aside  string
}

// Load replaces the state with the file's, once no write a Commit started is
// still running. A missing file leaves the state empty and reports nothing.
// An unreadable, oversized, corrupt or other-version file, one that is not a
// regular file, or one holding a run or verdict the store never writes, also
// leaves it empty: the file is renamed to Path plus ".corrupt-<unix seconds>"
// and reported, with an error when the rename failed. One whose ctx ends
// while it waits returns ctx's error and leaves the state as it was.
func (s *Store) Load(ctx context.Context) (*Discarded, error) {
	if !s.takeWrite(ctx) {
		return nil, ctx.Err()
	}
	defer s.endWrite()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = newState()
	path := s.Path()
	raw, err := s.read(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	var st *state
	if err == nil {
		st, err = decode(raw)
	}
	if err == nil {
		s.st = st
		return nil, nil
	}
	aside := path + ".corrupt-" + strconv.FormatInt(time.Now().Unix(), 10)
	if rerr := os.Rename(path, aside); rerr != nil {
		return &Discarded{Reason: err}, fmt.Errorf("set aside run state: %w", rerr)
	}
	return &Discarded{Reason: err, Aside: aside}, nil
}

// read reads the state file through an os.Root over the store directory, so
// the open cannot block on a FIFO planted at its name.
func (s *Store) read(ctx context.Context) ([]byte, error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readState(ctx, root, fileName, maxStateBytes)
}

// Commit hands deliver, which must not call Commit, the observations of r
// (see Read) that change conn's state, in order. Only when deliver reports
// them all delivered does it record r and write the whole state, so the file
// never holds an undelivered change. It returns by the end of ctx, cause
// wrapped, even while a write stalls, which runs on; one whose ctx ends while
// it waits behind that write delivers nothing. A write that fails or never
// runs leaves the file as it was and the change in memory, so a later write
// stores it and a restart before one delivers it again.
func (s *Store) Commit(ctx context.Context, conn string, r *Read, deliver func([]Change) bool) error {
	if !s.takeWrite(ctx) {
		return fmt.Errorf("write run state: %w", context.Cause(ctx))
	}
	next, changes, err := s.record(conn, r)
	if err == nil && !deliver(changes) {
		err = fmt.Errorf("write run state: %w", errUndelivered)
	}
	var data []byte
	if err == nil {
		data, err = s.adopt(next)
	}
	if err == nil && ctx.Err() != nil {
		err = fmt.Errorf("write run state: %w", context.Cause(ctx))
	}
	if err != nil {
		s.endWrite()
		return err
	}
	done := make(chan error, 1)
	go func() {
		err := s.write(ctx, data)
		// Before done, so a Close after Commit returns releases at once.
		s.endWrite()
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		select {
		case err := <-done:
			return err
		default:
			return fmt.Errorf("write run state: %w", context.Cause(ctx))
		}
	}
}

// record applies r on conn to a copy of the state and returns the copy and
// the changes, failing with errClosed once the Store is closed. The caller
// holds the right to write, so no other write lands between the copy and
// its adoption.
func (s *Store) record(conn string, r *Read) (*state, []Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, errClosed
	}
	next := s.st.clone()
	changes := next.apply(conn, r)
	return next, changes, nil
}

// adopt makes next the state and encodes it for the write, failing with
// errClosed, and adopting nothing, once the Store is closed.
func (s *Store) adopt(next *state) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed
	}
	s.st = next
	data, err := json.Marshal(s.st.file())
	if err != nil {
		return nil, fmt.Errorf("encode run state: %w", err)
	}
	return data, nil
}

// write replaces the file with data. ErrNotDurable reports a write in place
// that a power loss could still undo.
func (s *Store) write(ctx context.Context, data []byte) error {
	res, err := writeState(ctx, s.Path(), data, atomicfile.WithMode(0o600), atomicfile.WithMaxBytes(maxStateBytes))
	if err != nil {
		return err
	}
	if !res.Durable {
		return ErrNotDurable
	}
	return nil
}

// staleTempAge is how old an abandoned write's temp file must be to reclaim.
const staleTempAge = time.Hour

// CleanupTemps removes temp files a write interrupted by a crash left in the
// store directory, reporting how many could not be removed or inspected.
func (s *Store) CleanupTemps(ctx context.Context) (failed int, err error) {
	res, err := atomicfile.CleanupStaleTemps(ctx, s.dir, staleTempAge)
	return res.Failed + res.Unreadable, err
}

// Delivered is the number of runs whose delivered state the store holds for
// conn.
func (s *Store) Delivered(conn string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	if cs, ok := s.st.conns[conn]; ok {
		for _, byID := range cs.Delivered {
			n += len(byID)
		}
	}
	return n
}

// Workflows iterates conn's branch verdicts.
func (s *Store) Workflows(conn string) iter.Seq2[WorkflowKey, WorkflowState] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.All(s.st.workflows(conn))
}

// DayCount is one repository's runs created on one UTC day, Day spelled
// YYYY-MM-DD, counted once each in its newest state. Timed is the runs of the
// default branch with a known run time and Seconds their sum. Clipped reports
// a day part of which no listing covered, so it holds at least these runs.
type DayCount struct {
	Repo    string
	Day     string
	Seconds int64
	Passing int
	Failing int
	Neutral int
	Timed   int
	Clipped bool
}

// Days is conn's day counts, by repository then day.
func (s *Store) Days(conn string) []DayCount {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.st.conns[conn]
	if !ok {
		return nil
	}
	var out []DayCount
	for repo, byDay := range cs.Days {
		for key, d := range byDay {
			out = append(out, d.count(repo, key))
		}
	}
	slices.SortFunc(out, func(a, b DayCount) int { return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Day, b.Day)) })
	return out
}

// LastListed is the start of the last scan whose listing of conn's repo
// reached back to the one before it, leaving no span unlisted between them,
// or the first listing the store keeps of it; zero when it keeps none. A
// listing cut short of the one before leaves it in place until the gap it
// left is past the window.
func (s *Store) LastListed(conn, repo string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs, ok := s.st.conns[conn]; ok {
		return cs.Listed[repo]
	}
	return time.Time{}
}

// Pending is the creation time of the oldest run the last listing of conn's
// repo returned still pending, zero when it returned none.
func (s *Store) Pending(conn, repo string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs, ok := s.st.conns[conn]; ok {
		return cs.Pending[repo]
	}
	return time.Time{}
}

// Stranded reports a listing cut short at cover that does not reach a run the
// listing before it returned pending, created at pending: the run may finish
// unlisted. A whole listing never strands one; a run pending past the lookback
// is never delivered.
func Stranded(cut bool, cover, pending time.Time) bool {
	return cut && !pending.IsZero() && pending.Before(cover)
}

// Covered reports a listing holding every run created from cover on that
// follows, with no gap, one taken at prev: none was created between them
// unlisted. A listing that follows none is covered by nothing.
func Covered(prev, cover time.Time) bool { return !prev.IsZero() && !cover.After(prev) }

// Vouched reports whether a listing vouches for a verdict whose run was
// created at last: a listing cut short at cover does not for one older than
// cover, as the workflow may have run since, below the cut.
func Vouched(cut bool, cover, last time.Time) bool { return !cut || !last.Before(cover) }

// Read is what one connection's scan read.
type Read struct {
	// Start is the scan's start: a verdict with no run created in the
	// WorkflowRetention before it is dropped.
	Start time.Time
	// Window is the lookback window's start: a delivered run created before
	// it can be listed no more and is dropped.
	Window time.Time
	// Kept is the repositories a whole discovery of the connection kept,
	// whose verdicts alone stay; nil when discovery was not whole.
	Kept map[string]bool
	// Defined is, for each repository whose workflow definitions this scan
	// listed whole, the names it listed, deleted ones included.
	Defined  map[string][]string
	Listings []Listing
}

// Listing is one repository's run listing of the scan.
type Listing struct {
	// Cover is the creation time from which the listing holds every run:
	// the window's start, or the oldest row of a listing cut short.
	Cover time.Time
	// Pending is the creation time of the oldest run the listing returned
	// still pending, zero when it returned none.
	Pending time.Time
	Repo    string
	// Runs is the listing's completed runs the adapter mapped.
	Runs []Observation
	// Cut reports a listing cut short, which follows no earlier one: a run
	// created before its oldest row and still pending then may have finished
	// since unlisted.
	Cut bool
}

// Observation is one run a scan read.
type Observation struct {
	Run forge.Run
	// OnDefault is whether the run is of its repository's default branch,
	// itself and not a pull request's: the store counts only those runs in
	// a day's run time, and carries the flag to the change unread.
	OnDefault bool
}

// Change is an observation that changed a connection's state: a run seen for
// the first time (New), or one whose state or update time moved from
// Previous.
type Change struct {
	Previous forge.RunState
	Observation
	New bool
}

// WorkflowKey names one workflow of one repository on one branch.
type WorkflowKey struct {
	Repo   string
	Name   string
	Branch string
}

// WorkflowState is a workflow's newest judged run on a branch and its
// failure streak there. A run a pull request started is no run of the branch
// it names. Last is that run's creation time. Clipped reports a streak that
// may have begun before Since: its start reaches back past what any listing
// covered.
type WorkflowState struct {
	Since   time.Time
	Last    time.Time
	State   forge.RunState
	URL     string
	RunID   int64
	Streak  int
	Clipped bool
	// Defined reports a whole workflow listing of the repository that has
	// named the workflow since the store began holding its verdict.
	Defined bool
}

// state is the verdicts, delivered runs and days of every connection.
type state struct {
	conns map[string]*connState
}

func newState() *state { return &state{conns: map[string]*connState{}} }

type connState struct {
	// Verdicts holds each repository's verdicts by workflow then branch.
	Verdicts map[string]map[string]map[string]*verdict `json:"verdicts"`
	// Delivered holds each repository's delivered runs by ID, a run ID
	// identifying a run only within its repository, until the run is
	// created before the window.
	Delivered map[string]map[string]*delivered `json:"delivered"`
	// Listed holds, for each repository, the start of the last scan whose
	// listing reached back to the one before it, followed a gap past its
	// window or followed none; it is dropped once older than the retention.
	Listed map[string]time.Time `json:"listed,omitempty"`
	// Pending holds, for each repository, the creation time of the oldest
	// run its last listing returned still pending; dropped like Listed.
	Pending map[string]time.Time `json:"pending,omitempty"`
	// Days holds, for each repository, each UTC day of the last
	// dayRetention by its YYYY-MM-DD spelling.
	Days map[string]map[string]*day `json:"days,omitempty"`
}

// day is the runs created on one UTC day, by ID, each in its newest state.
// Clipped reports a day part of which no listing covered.
type day struct {
	Runs    map[string]*dayRun `json:"runs,omitempty"`
	Clipped bool               `json:"clipped,omitempty"`
}

// dayRun is one run's newest state and, when Timed, its run time in whole
// seconds, which counts in its day's run time.
type dayRun struct {
	State   forge.RunState `json:"state"`
	Seconds int64          `json:"seconds,omitempty"`
	Timed   bool           `json:"timed,omitempty"`
}

func (d *day) count(repo, key string) DayCount {
	c := DayCount{Repo: repo, Day: key, Clipped: d.Clipped}
	for _, r := range d.Runs {
		switch r.State {
		case forge.RunPassing:
			c.Passing++
		case forge.RunFailing:
			c.Failing++
		case forge.RunNeutral:
			c.Neutral++
		}
		if r.Timed {
			c.Timed++
			c.Seconds += r.Seconds
		}
	}
	return c
}

// delivered is the state of a run's last ci run line.
type delivered struct {
	Created time.Time      `json:"created"`
	Updated time.Time      `json:"updated"`
	State   forge.RunState `json:"state"`
}

// streak is the run of failures a verdict's run ends: none on a run that did
// not fail.
type streak struct {
	Since   time.Time `json:"since,omitzero"`
	Count   int       `json:"count,omitempty"`
	Clipped bool      `json:"clipped,omitempty"`
}

// verdict is a branch verdict as of one run: that run and the failure streak
// ending at it.
type verdict struct {
	Created time.Time      `json:"created"`
	State   forge.RunState `json:"state"`
	URL     string         `json:"url"`
	// Base is the streak the run followed, nil when no listing covered the
	// run before it; a re-run of the run is judged against it again.
	Base    *streak `json:"base,omitempty"`
	Streak  streak  `json:"streak"`
	RunID   int64   `json:"run_id"`
	Defined bool    `json:"defined,omitempty"`
}

type stateFile struct {
	Connections map[string]*connState `json:"connections"`
	Version     int                   `json:"version"`
}

func decode(raw []byte) (*state, error) {
	var f stateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decode run state: %w", err)
	}
	if f.Version != formatVersion {
		return nil, fmt.Errorf("run state format version %d, want %d", f.Version, formatVersion)
	}
	st := newState()
	for name, cs := range f.Connections {
		if cs == nil {
			continue
		}
		cs = cs.normalized()
		if err := cs.validate(); err != nil {
			return nil, fmt.Errorf("run state connection %q: %w", name, err)
		}
		st.conns[name] = cs
	}
	return st, nil
}

// Storable reports why the store cannot hold r: an ID that is not positive,
// a state outside the RunState members, no creation time, or an update time
// before it. A file holding such a run is corrupt.
func Storable(r *forge.Run) error {
	return validRun(r.ID, r.State, r.CreatedAt, r.UpdatedAt)
}

func validRun(id int64, state forge.RunState, created, updated time.Time) error {
	switch {
	case id <= 0:
		return fmt.Errorf("run id %d is not positive", id)
	case !validState(state):
		return fmt.Errorf("run %d state %q is not a run state", id, state)
	case created.IsZero():
		return fmt.Errorf("run %d has no creation time", id)
	case updated.Before(created):
		return fmt.Errorf("run %d was updated before it was created", id)
	}
	return nil
}

func validState(s forge.RunState) bool {
	return s == forge.RunPassing || s == forge.RunFailing || s == forge.RunNeutral
}

// validate rejects a decoded connection holding what apply never writes.
func (cs *connState) validate() error {
	if err := cs.validDelivered(); err != nil {
		return err
	}
	if err := cs.validVerdicts(); err != nil {
		return err
	}
	for repo, t := range cs.Listed {
		if t.IsZero() {
			return fmt.Errorf("repo %q was listed at no time", repo)
		}
	}
	for repo, t := range cs.Pending {
		if t.IsZero() {
			return fmt.Errorf("repo %q holds a pending run created at no time", repo)
		}
	}
	return cs.validDays()
}

func (cs *connState) validDays() error {
	for repo, byDay := range cs.Days {
		for key, d := range byDay {
			if t, err := time.Parse(time.DateOnly, key); err != nil || t.Format(time.DateOnly) != key {
				return fmt.Errorf("repo %q day %q is not a date", repo, key)
			}
			if err := d.validate(); err != nil {
				return fmt.Errorf("repo %q day %s: %w", repo, key, err)
			}
		}
	}
	return nil
}

func (d *day) validate() error {
	for id, r := range d.Runs {
		_, err := runID(id)
		switch {
		case err != nil:
			return err
		case !validState(r.State):
			return fmt.Errorf("run %s state %q is not a run state", id, r.State)
		case r.Seconds < 0 || (!r.Timed && r.Seconds != 0):
			return fmt.Errorf("run %s run time %d is negative or on an untimed run", id, r.Seconds)
		}
	}
	return nil
}

func (cs *connState) validDelivered() error {
	for repo, byID := range cs.Delivered {
		for key, d := range byID {
			id, err := runID(key)
			if err == nil {
				err = validRun(id, d.State, d.Created, d.Updated)
			}
			if err != nil {
				return fmt.Errorf("delivered repo %q: %w", repo, err)
			}
		}
	}
	return nil
}

func (cs *connState) validVerdicts() error {
	for repo, byName := range cs.Verdicts {
		for name, byBranch := range byName {
			for branch, v := range byBranch {
				if err := v.validate(); err != nil {
					return fmt.Errorf("repo %q workflow %q branch %q: %w", repo, name, branch, err)
				}
			}
		}
	}
	return nil
}

// runID parses a stored run key, refusing any spelling apply never writes.
func runID(key string) (int64, error) {
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil || strconv.FormatInt(id, 10) != key {
		return 0, fmt.Errorf("run key %q is not a run id", key)
	}
	return id, nil
}

// validate holds a verdict to judge's shape: its base is no streak, the zero
// streak, or a failure streak begun no later than its run, and its streak is
// the one its run ends after that base.
func (v *verdict) validate() error {
	want := v.Base.next(v.State, v.Created)
	switch {
	case v.RunID <= 0:
		return fmt.Errorf("run id %d is not positive", v.RunID)
	case !validState(v.State):
		return fmt.Errorf("state %q is not a run state", v.State)
	case v.Created.IsZero():
		return errors.New("verdict has no run creation time")
	case v.Base != nil && *v.Base != (streak{}) && (v.Base.Count < 1 || v.Base.Since.IsZero() || v.Base.Since.After(v.Created)):
		return errors.New("verdict base is no failure streak begun before its run")
	case !v.Streak.equal(&want):
		return fmt.Errorf("%s verdict streak %d since %s is not the one its run ends", v.State, v.Streak.Count, v.Streak.Since.Format(time.RFC3339))
	}
	return nil
}

func (b *streak) equal(o *streak) bool {
	return b.Count == o.Count && b.Clipped == o.Clipped && b.Since.Equal(o.Since)
}

func (cs *connState) normalized() *connState {
	if cs.Verdicts == nil {
		cs.Verdicts = map[string]map[string]map[string]*verdict{}
	}
	if cs.Delivered == nil {
		cs.Delivered = map[string]map[string]*delivered{}
	}
	if cs.Listed == nil {
		cs.Listed = map[string]time.Time{}
	}
	if cs.Pending == nil {
		cs.Pending = map[string]time.Time{}
	}
	if cs.Days == nil {
		cs.Days = map[string]map[string]*day{}
	}
	cs.dropEmptyDays()
	for repo, byID := range cs.Delivered {
		maps.DeleteFunc(byID, func(_ string, d *delivered) bool { return d == nil })
		if len(byID) == 0 {
			delete(cs.Delivered, repo)
		}
	}
	cs.dropVerdicts(func(_ WorkflowKey, v *verdict) bool { return v == nil })
	return cs
}

func (st *state) file() stateFile {
	return stateFile{Version: formatVersion, Connections: st.conns}
}

// clone is a deep copy of st, which apply can change while st stays as it
// was.
func (st *state) clone() *state {
	out := newState()
	for name, cs := range st.conns {
		out.conns[name] = cs.clone()
	}
	return out
}

func (cs *connState) clone() *connState {
	out := &connState{
		Verdicts:  make(map[string]map[string]map[string]*verdict, len(cs.Verdicts)),
		Delivered: make(map[string]map[string]*delivered, len(cs.Delivered)),
		Listed:    maps.Clone(cs.Listed),
		Pending:   maps.Clone(cs.Pending),
	}
	for repo, byName := range cs.Verdicts {
		names := make(map[string]map[string]*verdict, len(byName))
		for name, byBranch := range byName {
			branches := make(map[string]*verdict, len(byBranch))
			for branch, v := range byBranch {
				c := *v
				if v.Base != nil {
					base := *v.Base
					c.Base = &base
				}
				branches[branch] = &c
			}
			names[name] = branches
		}
		out.Verdicts[repo] = names
	}
	for repo, byID := range cs.Delivered {
		runs := make(map[string]*delivered, len(byID))
		for id, d := range byID {
			c := *d
			runs[id] = &c
		}
		out.Delivered[repo] = runs
	}
	out.Days = make(map[string]map[string]*day, len(cs.Days))
	for repo, byDay := range cs.Days {
		out.Days[repo] = cloneDays(byDay)
	}
	return out.normalized()
}

func cloneDays(byDay map[string]*day) map[string]*day {
	out := make(map[string]*day, len(byDay))
	for key, d := range byDay {
		out[key] = d.clone()
	}
	return out
}

func (d *day) clone() *day {
	c := &day{Clipped: d.Clipped, Runs: make(map[string]*dayRun, len(d.Runs))}
	for id, r := range d.Runs {
		rc := *r
		c.Runs[id] = &rc
	}
	return c
}

// dropEmptyDays deletes the nil days and runs a decoded file can hold, and
// every repository left with no day.
func (cs *connState) dropEmptyDays() {
	for repo, byDay := range cs.Days {
		maps.DeleteFunc(byDay, func(_ string, d *day) bool { return d == nil })
		for _, d := range byDay {
			maps.DeleteFunc(d.Runs, func(_ string, r *dayRun) bool { return r == nil })
		}
		if len(byDay) == 0 {
			delete(cs.Days, repo)
		}
	}
}

func (st *state) conn(name string) *connState {
	cs, ok := st.conns[name]
	if !ok {
		cs = (&connState{}).normalized()
		st.conns[name] = cs
	}
	return cs
}

// apply records r on conn and returns the observations that changed it, in
// each listing's oldest-first order: a run never delivered, or one whose
// state or update time differs from its last delivery. A run Storable
// refuses, and a reading older than the last delivery, are not recorded. A
// whole discovery forgets the verdicts of the repositories it did not keep.
// Last, every connection drops what the window and the retention no longer
// hold.
func (st *state) apply(conn string, r *Read) []Change {
	cs := st.conn(conn)
	if r.Kept != nil {
		cs.keep(r.Kept)
	}
	changes := make([][]Change, len(r.Listings))
	for i := range r.Listings {
		changes[i] = cs.list(&r.Listings[i], r.Start, r.Window)
	}
	for repo, names := range r.Defined {
		cs.define(repo, names)
	}
	for _, c := range st.conns {
		c.prune(r.Window, r.Start)
	}
	return slices.Concat(changes...)
}

// list judges and delivers one listing's runs, oldest first, and counts each
// on the day it was created.
func (cs *connState) list(l *Listing, start, window time.Time) []Change {
	contiguous := cs.relist(l, start, window)
	runs := slices.Clone(l.Runs)
	slices.SortFunc(runs, func(a, b Observation) int {
		return cmp.Or(a.Run.CreatedAt.Compare(b.Run.CreatedAt), cmp.Compare(a.Run.ID, b.Run.ID))
	})
	judged := map[WorkflowKey]bool{}
	out := make([]Change, 0, len(runs))
	for i := range runs {
		o := &runs[i]
		if Storable(&o.Run) != nil {
			continue
		}
		ch, fresh, changed := cs.deliver(&o.Run)
		if !fresh {
			continue
		}
		cs.countDay(l.Repo, o, start)
		if o.Run.Trigger != forge.TriggerPullRequest {
			k := WorkflowKey{Repo: l.Repo, Name: o.Run.Workflow, Branch: o.Run.Branch}
			v := cs.verdict(k)
			// A verdict whose run this listing covers has no gap before the
			// runs that follow it.
			covered := contiguous || judged[k] || (v != nil && !v.Created.Before(l.Cover))
			if next := judge(v, &o.Run, covered); next != nil {
				cs.setVerdict(k, next)
				judged[k] = true
			}
		}
		if changed {
			ch.Observation = *o
			out = append(out, ch)
		}
	}
	return out
}

// relist records l, taken at start, as its repository's listing, with the
// oldest run it returned pending, and reports whether its runs follow the
// previous listing with no gap. The listing time
// moves to start unless l is cut short of a previous listing still inside
// window: the span between them, which nobody listed, then stays a gap for
// the next listing to report. A first listing, cut or not, sets it, as the
// runs it did not reach predate the store. Every day a gap reaches, and the
// day of a pending run l cuts off, is clipped.
func (cs *connState) relist(l *Listing, start, window time.Time) (contiguous bool) {
	prev := cs.Listed[l.Repo]
	covered := Covered(prev, l.Cover)
	if !l.Cut || covered || !Covered(prev, window) {
		cs.Listed[l.Repo] = start
		if !covered {
			cs.clip(l.Repo, cmp.Or(prev, dayStart(l.Cover)), l.Cover, start)
		}
	}
	if p := cs.Pending[l.Repo]; Stranded(l.Cut, l.Cover, p) {
		cs.clip(l.Repo, p, p.Add(time.Nanosecond), start)
	}
	if l.Pending.IsZero() {
		delete(cs.Pending, l.Repo)
	} else {
		cs.Pending[l.Repo] = l.Pending
	}
	return !l.Cut && covered
}

// deliver records r as delivered: fresh is false for a reading older than
// the last delivery, which records nothing, and changed reports a run never
// delivered or one whose state or update time moved.
func (cs *connState) deliver(r *forge.Run) (ch Change, fresh, changed bool) {
	key := strconv.FormatInt(r.ID, 10)
	prev, seen := cs.Delivered[r.Repo][key]
	if seen && r.UpdatedAt.Before(prev.Updated) {
		return Change{}, false, false
	}
	if seen && prev.State == r.State && prev.Updated.Equal(r.UpdatedAt) {
		return Change{}, true, false
	}
	byID := cs.Delivered[r.Repo]
	if byID == nil {
		byID = map[string]*delivered{}
		cs.Delivered[r.Repo] = byID
	}
	byID[key] = &delivered{Created: r.CreatedAt, Updated: r.UpdatedAt, State: r.State}
	ch = Change{New: !seen}
	if seen {
		ch.Previous = prev.State
	}
	return ch, true, true
}

// judge is the verdict after r on v's branch, nil when r leaves v as it is:
// a run older than v's, or v's own run read in the state v holds. covered
// says no run of the workflow there was created unlisted between v's run and
// r; a streak whose start no listing covered is clipped.
func judge(v *verdict, r *forge.Run, covered bool) *verdict {
	var base *streak
	switch {
	case v == nil:
	case v.RunID == r.ID:
		if v.State == r.State {
			return nil
		}
		base = v.Base
	case !v.before(r.CreatedAt, r.ID):
		return nil
	case covered:
		b := v.Streak
		base = &b
	}
	return &verdict{
		Created: r.CreatedAt, State: r.State, URL: r.URL, RunID: r.ID,
		Base: base, Streak: base.next(r.State, r.CreatedAt), Defined: v != nil && v.Defined,
	}
}

// next is the streak a run in state s created at created ends, following b;
// a nil b is a streak no listing covered the start of.
func (b *streak) next(s forge.RunState, created time.Time) streak {
	switch {
	case s != forge.RunFailing:
		return streak{}
	case b == nil:
		return streak{Count: 1, Since: created, Clipped: true}
	case b.Count > 0:
		return streak{Count: b.Count + 1, Since: b.Since, Clipped: b.Clipped}
	default:
		return streak{Count: 1, Since: created}
	}
}

// before reports whether v's run precedes the run id created at created, in
// the creation-then-ID order runs are judged in.
func (v *verdict) before(created time.Time, id int64) bool {
	if created.Equal(v.Created) {
		return id > v.RunID
	}
	return created.After(v.Created)
}

// keep forgets the verdicts, listing times, pending runs and days of every
// repository not in kept; their delivered runs stay until they leave the
// window, so a repository that returns does not report them again.
func (cs *connState) keep(kept map[string]bool) {
	maps.DeleteFunc(cs.Verdicts, func(repo string, _ map[string]map[string]*verdict) bool { return !kept[repo] })
	notKept := func(repo string, _ time.Time) bool { return !kept[repo] }
	maps.DeleteFunc(cs.Listed, notKept)
	maps.DeleteFunc(cs.Pending, notKept)
	maps.DeleteFunc(cs.Days, func(repo string, _ map[string]*day) bool { return !kept[repo] })
}

// dayStart is the UTC midnight that begins t's day.
func dayStart(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// firstDay is the oldest day a scan started at start keeps.
func firstDay(start time.Time) time.Time { return dayStart(start).AddDate(0, 0, 1-dayRetention) }

// countDay records o as its run's newest state on the day it was created,
// once that day is retained.
func (cs *connState) countDay(repo string, o *Observation, start time.Time) {
	created := dayStart(o.Run.CreatedAt)
	if created.Before(firstDay(start)) {
		return
	}
	r := &dayRun{State: o.Run.State}
	if d, ok := o.Run.Duration(); ok && o.OnDefault {
		r.Timed, r.Seconds = true, int64(d/time.Second)
	}
	cs.day(repo, created.Format(time.DateOnly)).Runs[strconv.FormatInt(o.Run.ID, 10)] = r
}

// clip marks clipped each retained day of repo the unlisted span [from, to)
// reaches.
func (cs *connState) clip(repo string, from, to, start time.Time) {
	d := dayStart(from)
	if first := firstDay(start); d.Before(first) {
		d = first
	}
	for ; d.Before(to); d = d.AddDate(0, 0, 1) {
		cs.day(repo, d.Format(time.DateOnly)).Clipped = true
	}
}

func (cs *connState) day(repo, key string) *day {
	byDay := cs.Days[repo]
	if byDay == nil {
		byDay = map[string]*day{}
		cs.Days[repo] = byDay
	}
	d := byDay[key]
	if d == nil {
		d = &day{Runs: map[string]*dayRun{}}
		byDay[key] = d
	}
	if d.Runs == nil {
		d.Runs = map[string]*dayRun{}
	}
	return d
}

// define marks each of repo's verdicts of a workflow names lists.
func (cs *connState) define(repo string, names []string) {
	for _, name := range names {
		for _, v := range cs.Verdicts[repo][name] {
			v.Defined = true
		}
	}
}

// prune drops the delivered runs created before window, which no listing
// reads again, the verdicts, listing times and pending runs older than the
// retention before start, and the days before the dayRetention ending on
// start's.
func (cs *connState) prune(window, start time.Time) {
	for repo, byID := range cs.Delivered {
		maps.DeleteFunc(byID, func(_ string, d *delivered) bool { return d.Created.Before(window) })
		if len(byID) == 0 {
			delete(cs.Delivered, repo)
		}
	}
	first := firstDay(start).Format(time.DateOnly)
	for repo, byDay := range cs.Days {
		maps.DeleteFunc(byDay, func(key string, _ *day) bool { return key < first })
		if len(byDay) == 0 {
			delete(cs.Days, repo)
		}
	}
	floor := start.Add(-WorkflowRetention)
	cs.dropVerdicts(func(_ WorkflowKey, v *verdict) bool { return v.Created.Before(floor) })
	old := func(_ string, t time.Time) bool { return t.Before(floor) }
	maps.DeleteFunc(cs.Listed, old)
	maps.DeleteFunc(cs.Pending, old)
}

func (cs *connState) verdict(k WorkflowKey) *verdict {
	return cs.Verdicts[k.Repo][k.Name][k.Branch]
}

func (cs *connState) setVerdict(k WorkflowKey, v *verdict) {
	byName := cs.Verdicts[k.Repo]
	if byName == nil {
		byName = map[string]map[string]*verdict{}
		cs.Verdicts[k.Repo] = byName
	}
	byBranch := byName[k.Name]
	if byBranch == nil {
		byBranch = map[string]*verdict{}
		byName[k.Name] = byBranch
	}
	byBranch[k.Branch] = v
}

// dropVerdicts deletes the verdicts drop selects, and every workflow and
// repository left with none.
func (cs *connState) dropVerdicts(drop func(WorkflowKey, *verdict) bool) {
	for repo, byName := range cs.Verdicts {
		for name, byBranch := range byName {
			maps.DeleteFunc(byBranch, func(branch string, v *verdict) bool {
				return drop(WorkflowKey{Repo: repo, Name: name, Branch: branch}, v)
			})
			if len(byBranch) == 0 {
				delete(byName, name)
			}
		}
		if len(byName) == 0 {
			delete(cs.Verdicts, repo)
		}
	}
}

func (st *state) workflows(conn string) map[WorkflowKey]WorkflowState {
	out := map[WorkflowKey]WorkflowState{}
	if cs, ok := st.conns[conn]; ok {
		for repo, byName := range cs.Verdicts {
			for name, byBranch := range byName {
				for branch, v := range byBranch {
					out[WorkflowKey{Repo: repo, Name: name, Branch: branch}] = WorkflowState{
						Since: v.Streak.Since, Last: v.Created, State: v.State, URL: v.URL, RunID: v.RunID,
						Streak: v.Streak.Count, Clipped: v.Streak.Clipped, Defined: v.Defined,
					}
				}
			}
		}
	}
	return out
}
