package collect

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/runstate"
)

// scan is what every connection of one scan shares.
type scan struct {
	start  time.Time
	cutoff time.Time
	state  *runstate.Store
	// out emits the burst; it refuses every line once the scan's context
	// has ended, which makes the scan Interrupted.
	out   emitter
	gates []*accountGate
	// id is the scan start in Unix milliseconds, unique because one process
	// owns the run state and runs its scans one after another.
	id int64
}

// stopInfo is a connection whose scan stopped before its last read.
type stopInfo struct {
	reason string
	phase  string
}

// connResult is one connection's reads, held for the burst.
type connResult struct {
	log *slog.Logger
	// kept is the repositories every signal reads, by lowercased path;
	// dropped the archived and excluded ones discovery answered.
	kept    map[string]forge.Repo
	dropped map[string]bool
	// byOwner is each owner's kept repositories as discovery answered them,
	// path-sorted: the projects a per-repository fallback reads.
	byOwner map[string][]forge.Repo
	// runLists and workflowLists are each repository's run and workflow
	// listing as read; approve turns them into what the scan stores and
	// publishes.
	runLists      []runListing
	workflowLists []workflowListing
	// defined is the workflow definitions of each repository GitHub listed
	// whole, by lowercased path.
	defined map[string]*definitions
	// keptWhole is the repositories a complete discovery kept, by path, empty
	// when every owner resolved and listed none; nil when discovery was cut
	// short, failed or left an owner unresolved.
	keptWhole map[string]bool
	// unmapped is the runs in a state forgeapi could not map, by repository
	// path and ID.
	unmapped map[string]bool
	// seen holds the pull requests and issues already kept, by kind, path and
	// number, since a parent group's listing includes its subgroups'.
	seen map[string]bool
	// ownersEmpty is the configured owners a whole discovery answered with
	// no repository at all.
	ownersEmpty []string
	cfg         *config.Connection
	budget      forge.Budget
	login       string
	// phase is the read under way, which a stop records.
	phase    string
	prs      []prRow
	issues   []issueRow
	alerts   []forge.Alert
	disabled []forge.Workflow
	failing  []failingRow
	slow     []slowRow
	days     []runstate.DayCount
	ledger   readLedger
	sum      summary
	product  forge.Product
}

type summary struct {
	duration                                     time.Duration
	reposDiscovered                              int
	newRuns, changedRuns, newFailures, delivered int
	runDurationsKnown                            int
}

// The read phases a scan stopped line names.
const (
	phaseOpen      = "open"
	phaseDiscovery = "discovery"
	phasePRs       = "open pull requests"
	phaseIssues    = "open issues"
	phaseRuns      = "runs"
	phaseChecks    = "pr checks"
	phaseSecurity  = "security"
	phaseWorkflows = "workflows"
	phaseState     = "run state"
)

// errStopped ends a connection's reads; the ledger already holds the stop.
var errStopped = errors.New("connection scan stopped")

func (c *Collector) scanConn(ctx context.Context, sc *scan, cs *connState) *connResult {
	r := &connResult{
		cfg: &cs.cfg, kept: map[string]forge.Repo{}, dropped: map[string]bool{}, byOwner: map[string][]forge.Repo{},
		defined: map[string]*definitions{}, seen: map[string]bool{}, unmapped: map[string]bool{},
		budget: forge.Budget{Remaining: -1}, phase: phaseOpen,
	}
	r.log = Scope(c.logger, forge.ProductUnknown, cs.cfg.Name)
	opened := c.readConn(ctx, sc, cs, r)
	if r.ledger.stop == nil && errors.Is(context.Cause(ctx), errScanTimeout) {
		r.ledger.timedOut = true
		r.ledger.halt(stopScanTimeout, r.phase)
	}
	if !opened {
		return r
	}
	r.budget = cs.conn.Budget()
	if cs.gh != nil {
		r.budget = cs.gh.Budget()
	}
	r.approve()
	return r
}

// readConn opens the connection when needed and runs its reads in order,
// noting each step's phase, and reports whether the connection is open.
func (c *Collector) readConn(ctx context.Context, sc *scan, cs *connState, r *connResult) bool {
	opened, err := c.ensureOpen(ctx, cs)
	gate := sc.gates[cs.idx]
	if err != nil {
		gate.arrive(cs.idx, "")
		c.openFailure(ctx, r, err)
		return false
	}
	if cs.gh != nil {
		cs.gh.BeginScan()
	}
	c.named(r, cs.conn.Product())
	r.login = cs.conn.Login()
	gate.arrive(cs.idx, r.login)
	first, err := gate.first(ctx, cs.idx, r.login)
	if err != nil {
		return true
	}
	if first >= 0 {
		c.sharedAccount(ctx, cs, r, c.conns[first].cfg.Name)
		return false
	}
	// A connection reused from an earlier scan proves nothing about its token
	// until a read of this scan succeeds.
	r.ledger.opened = opened
	steps := []struct {
		read  func(context.Context, *scan, *connState, *connResult) error
		phase string
	}{
		{c.discover, phaseDiscovery},
		{c.readSnapshots, phasePRs},
		{c.readRuns, phaseRuns},
		{c.readChecks, phaseChecks},
		{c.readGitHub, phaseSecurity},
	}
	for _, step := range steps {
		r.phase = step.phase
		// With no repository visible, every row a later read returns is dropped.
		if err := step.read(ctx, sc, cs, r); err != nil || r.ledger.noRepos() {
			break
		}
	}
	return true
}

// sharedAccount closes a connection opened on the account an earlier one
// reads: two would each spend that account's request budget unaware of the
// other's.
func (c *Collector) sharedAccount(ctx context.Context, cs *connState, r *connResult, earlier string) {
	cs.conn.Close()
	cs.conn, cs.gh = nil, nil
	err := fmt.Errorf("%w: connection %q uses the same account as %q on %s; list both owners in one connection",
		forge.ErrConnection, cs.cfg.Name, earlier, cs.cfg.URL)
	c.openFailure(ctx, r, &forge.OpenError{Product: r.product, Err: err})
}

// named scopes the connection's lines to product p and records the families
// p does not have, before any read can fail or stop.
func (c *Collector) named(r *connResult, p forge.Product) {
	r.product = p
	r.log = Scope(c.logger, p, r.cfg.Name)
	r.ledger.classify(p, r.cfg.Security.Enabled)
}

// approve is the one gate between what the connection read and what its scan
// stores and publishes: a unit the ledger does not hold whole, a family or
// one repository's listing of it, contributes no row, no field value and no
// stored verdict. A blind or cut snapshot family emits nothing, never a
// smaller snapshot.
func (r *connResult) approve() {
	if r.ledger.withheld(famPRs) {
		r.prs = nil
	}
	if r.ledger.withheld(famIssues) {
		r.issues = nil
	}
	if r.ledger.withheld(famSecurity) {
		r.alerts = nil
	}
	if r.ledger.fam[famChecks].begun {
		for i := range r.prs {
			r.prs[i].checks = r.prs[i].verdict()
		}
	}
	keepDisabled := !r.ledger.withheld(famWorkflows)
	for i := range r.workflowLists {
		r.approveWorkflows(&r.workflowLists[i], keepDisabled)
	}
}

// definitions is one repository's workflow names, as a whole listing gave
// them.
type definitions struct {
	live, deleted map[string]bool
	path          string
}

// approveWorkflows keeps l's disabled workflows when the family publishes,
// and the names l defines when it was read whole.
func (r *connResult) approveWorkflows(l *workflowListing, keepDisabled bool) {
	var defs *definitions
	if l.whole {
		defs = &definitions{live: map[string]bool{}, deleted: map[string]bool{}, path: l.repo.Path}
		r.defined[strings.ToLower(l.repo.Path)] = defs
	}
	for j := range l.defs {
		w := &l.defs[j]
		if keepDisabled && w.Disabled() {
			r.disabled = append(r.disabled, *w)
		}
		switch {
		case defs == nil:
		case w.Deleted():
			defs.deleted[w.Name] = true
		default:
			defs.live[w.Name] = true
		}
	}
}

// gone reports a workflow d proves removed: listed deleted, or absent from
// it after an earlier whole listing named it. A run's name need not be its
// definition's (GitHub's dynamic workflows title each run), so a name never
// listed is never gone by absence.
func (d *definitions) gone(name string, listedBefore bool) bool {
	return !d.live[name] && (d.deleted[name] || listedBefore)
}

// verdict is the row's checks field: unknown unless its read was whole, as
// an omitted check may be failing.
func (row *prRow) verdict() forge.CheckState {
	if row.check == nil || row.check.End != forge.EndWhole {
		return forge.CheckUnknown
	}
	return row.check.State
}

// openFailure records a connection that did not open, under the product and
// with the budget it reported before failing when it reported them.
func (c *Collector) openFailure(ctx context.Context, r *connResult, err error) {
	if oe, ok := errors.AsType[*forge.OpenError](err); ok {
		c.named(r, oe.Product)
		if oe.Budget != nil {
			r.budget = *oe.Budget
		}
	}
	if r.stopped(err, famRepos) || isShutdown(ctx) {
		return
	}
	r.ledger.record(famRepos, 0, err)
	r.ledger.connFailed = true
	r.log.Error("repo discovery failed", "cause", failureCause(&r.ledger, err), "error", errText(err))
}

// failureCause is the cause the ERROR line of a failed open or discovery
// names: a 401 is a rejected token whether or not it is pervasive yet,
// anything else the escalated diagnosis.
func failureCause(l *readLedger, err error) string {
	if errors.Is(err, forge.ErrTokenInvalid) {
		return "token_invalid"
	}
	cause, _ := l.diagnosis()
	return cause
}

// stopped records err as the connection's stop in the read under way when it
// is one. A forge refusing further requests also fails the read of f; a read
// held back to keep the reserve fails nothing.
func (r *connResult) stopped(err error, f family) bool {
	reason, stop := stopErr(err)
	if !stop {
		return false
	}
	if reason == stopRateLimited {
		r.ledger.record(f, 0, err)
	}
	r.ledger.halt(reason, r.phase)
	return true
}

func (c *Collector) discover(ctx context.Context, sc *scan, cs *connState, r *connResult) error {
	r.ledger.begin(famRepos)
	d, err := cs.conn.Discover(ctx, cs.cfg.Owners)
	if err != nil {
		return c.discoveryFailure(ctx, r, err)
	}
	r.ledger.record(famRepos, d.End, nil)
	if len(d.Unresolved) > 0 {
		r.ledger.unresolved = append(r.ledger.unresolved, d.Unresolved...)
		r.ledger.drop(famRepos, dropUnresolved, len(d.Unresolved))
	}
	answered := false
	for _, owner := range cs.cfg.Owners {
		repos := d.ByOwner[owner]
		answered = answered || !slices.Contains(d.Unresolved, owner)
		if len(repos) == 0 && d.End == forge.EndWhole && !slices.Contains(d.Unresolved, owner) {
			r.ownersEmpty = append(r.ownersEmpty, owner)
		}
		r.byOwner[owner] = r.keepRepos(repos)
	}
	c.logEmptyOwners(cs, r)
	// An owner that resolved and listed nothing was read whole: only a
	// discovery that resolved no owner at all saw nothing.
	noRepos := !answered
	if noRepos {
		r.ledger.drop(famRepos, dropNoRepos, 1)
	}
	r.sum.reposDiscovered = len(r.kept)
	r.ledger.finish(famRepos)
	r.log.Info("scanning", "owners", len(cs.cfg.Owners), "repos", len(r.kept), "since", sc.cutoff.UTC().Format(time.RFC3339))
	if d.End == forge.EndWhole && len(d.Unresolved) == 0 {
		r.keepWhole()
	}
	if cs.cfg.SecuritySet && r.product != forge.ProductGitHub {
		r.log.Warn("security settings ignored: code scanning is GitHub-only")
	}
	return nil
}

// logEmptyOwners names the owners that resolved with no repository, at WARN
// the first scan the set is what it is and at DEBUG while it stays so: the
// read is complete, as there is nothing to read, but a misspelt owner that
// names another account looks just like it.
func (*Collector) logEmptyOwners(cs *connState, r *connResult) {
	empty := strings.Join(r.ownersEmpty, ",")
	if empty != "" {
		level := slog.LevelWarn
		if empty == cs.lastEmpty {
			level = slog.LevelDebug
		}
		r.log.Log(context.Background(), level, "owner lists no repository", "owners", empty)
	}
	cs.lastEmpty = empty
}

func (*Collector) discoveryFailure(ctx context.Context, r *connResult, err error) error {
	if r.stopped(err, famRepos) || isShutdown(ctx) {
		return errStopped
	}
	r.ledger.record(famRepos, 0, err)
	// A failed listing blanks every signal, so it escalates even on a 401 a
	// fresh open makes non-pervasive; a reused connection stays token_invalid.
	r.ledger.connFailed = true
	r.log.Error("repo discovery failed", "cause", failureCause(&r.ledger, err), "error", errText(err))
	return err
}

// keepRepos keeps the repositories every signal reads, not archived and not
// excluded, and returns them path-sorted. A repository a second owner lists
// again, as a parent group lists its subgroups', is counted once.
func (r *connResult) keepRepos(repos []forge.Repo) []forge.Repo {
	var out []forge.Repo
	for _, repo := range repos {
		key := strings.ToLower(repo.Path)
		if k, ok := r.kept[key]; ok {
			out = append(out, k)
			continue
		}
		if r.dropped[key] {
			continue
		}
		switch {
		case repo.Archived:
			r.dropped[key] = true
			r.ledger.drop(famRepos, dropArchived, 1)
		case r.cfg.ExcludeRepos[key]:
			r.dropped[key] = true
			r.ledger.drop(famRepos, dropExcludedRepo, 1)
			r.log.Debug("skipping excluded repo", "repo", bounded(repo.Path))
		default:
			r.kept[key] = repo
			out = append(out, repo)
		}
	}
	slices.SortFunc(out, byPath)
	return out
}

// keepWhole records the repositories a complete discovery kept, whose stored
// runs and workflows the save keeps and no other's.
func (r *connResult) keepWhole() {
	r.keptWhole = make(map[string]bool, len(r.kept))
	for _, repo := range r.kept {
		r.keptWhole[repo.Path] = true
	}
}

// sortedRepos is the kept repositories in path order.
func (r *connResult) sortedRepos() []forge.Repo {
	out := make([]forge.Repo, 0, len(r.kept))
	for _, repo := range r.kept {
		out = append(out, repo)
	}
	slices.SortFunc(out, byPath)
	return out
}

func byPath(a, b forge.Repo) int { return cmp.Compare(a.Path, b.Path) }

func (c *Collector) readSnapshots(ctx context.Context, _ *scan, cs *connState, r *connResult) error {
	r.ledger.begin(famPRs)
	r.ledger.begin(famIssues)
	for _, owner := range cs.cfg.Owners {
		if slices.Contains(r.ledger.unresolved, owner) {
			continue
		}
		// Once a family is blind its rows are dropped, so it is read no further.
		if !r.ledger.blind(famPRs) {
			if err := c.ownerPRs(ctx, cs, r, owner); err != nil {
				return err
			}
		}
		if !r.ledger.blind(famIssues) {
			if err := c.ownerIssues(ctx, cs, r, owner); err != nil {
				return err
			}
		}
	}
	r.ledger.finish(famPRs)
	r.ledger.finish(famIssues)
	return nil
}

func (c *Collector) ownerPRs(ctx context.Context, cs *connState, r *connResult, owner string) error {
	r.phase = phasePRs
	prs, err := cs.conn.OpenPRs(ctx, owner)
	if errors.Is(err, forge.ErrOwnerUnresolved) {
		return c.repoPRs(ctx, cs, r, owner)
	}
	if err != nil {
		return c.snapshotFailure(ctx, r, famPRs, err, "owner", owner)
	}
	r.ledger.record(famPRs, forge.EndWhole, nil)
	r.addPRs(prs)
	return nil
}

func (c *Collector) ownerIssues(ctx context.Context, cs *connState, r *connResult, owner string) error {
	r.phase = phaseIssues
	issues, err := cs.conn.OpenIssues(ctx, owner)
	if errors.Is(err, forge.ErrOwnerUnresolved) {
		return c.repoIssues(ctx, cs, r, owner)
	}
	if err != nil {
		return c.snapshotFailure(ctx, r, famIssues, err, "owner", owner)
	}
	r.ledger.record(famIssues, forge.EndWhole, nil)
	r.addIssues(issues)
	return nil
}

// snapshotFailure records a failed listing, in the read under way, of an
// owner or of one project of the per-repository fallback (scope names which):
// the owner's snapshot is incomplete, so the family is blind, never a
// smaller snapshot.
func (*Collector) snapshotFailure(ctx context.Context, r *connResult, f family, err error, scope, name string) error {
	if r.stopped(err, f) || isShutdown(ctx) {
		return errStopped
	}
	failed := r.ledger.record(f, 0, err) == outcomeFailed
	r.ledger.drop(f, dropOwnerFailed, 1)
	if failed {
		r.warnRead(r.phase+" listing failed", scope, name, err, hintRefused)
	}
	return nil
}

// repoPRs is the per-repository fallback for an owner the instance's
// owner-wide listing does not resolve (a GitLab user namespace).
func (c *Collector) repoPRs(ctx context.Context, cs *connState, r *connResult, owner string) error {
	for _, repo := range r.byOwner[owner] {
		prs, err := cs.conn.RepoOpenPRs(ctx, repo)
		if err != nil {
			return c.snapshotFailure(ctx, r, famPRs, err, "repo", repo.Path)
		}
		r.ledger.record(famPRs, forge.EndWhole, nil)
		r.addPRs(prs)
	}
	return nil
}

func (c *Collector) repoIssues(ctx context.Context, cs *connState, r *connResult, owner string) error {
	for _, repo := range r.byOwner[owner] {
		issues, err := cs.conn.RepoOpenIssues(ctx, repo)
		if err != nil {
			return c.snapshotFailure(ctx, r, famIssues, err, "repo", repo.Path)
		}
		r.ledger.record(famIssues, forge.EndWhole, nil)
		r.addIssues(issues)
	}
	return nil
}

// excluded reports a row whose author or a label is in the noise lists.
func excluded(cfg *config.Connection, author string, labels []string) bool {
	if cfg.ExcludeAuthors[strings.ToLower(author)] {
		return true
	}
	return slices.ContainsFunc(labels, func(l string) bool { return cfg.ExcludeLabels[strings.ToLower(l)] })
}

// firstSeen reports an item not kept yet on this connection, and marks it.
func (r *connResult) firstSeen(kind, repo string, number int) bool {
	key := kind + " " + strings.ToLower(repo) + "#" + strconv.Itoa(number)
	if r.seen[key] {
		return false
	}
	r.seen[key] = true
	return true
}

// inKept reports an item of a kept repository. One of an archived or
// excluded repository is excluded with it; one of a repository discovery
// never answered leaves the family short.
func (r *connResult) inKept(f family, kind, repo string) bool {
	key := strings.ToLower(repo)
	if _, ok := r.kept[key]; ok {
		return true
	}
	if r.dropped[key] {
		r.ledger.drop(f, dropExcludedRepo, 1)
		return false
	}
	r.ledger.drop(f, dropUnlisted, 1)
	r.log.Debug("item outside the listed repositories", "kind", kind, "repo", bounded(repo))
	return false
}

func (r *connResult) addPRs(prs []forge.PullRequest) {
	for i := range prs {
		pr := &prs[i]
		if !r.inKept(famPRs, "pull request", pr.Repo) || !r.firstSeen("pr", pr.Repo, pr.Number) {
			continue
		}
		if excluded(r.cfg, pr.Author, pr.Labels) {
			r.ledger.drop(famPRs, dropExcludedItem, 1)
			r.logExcluded("pull request", pr.Repo, pr.Author, pr.Labels)
			continue
		}
		r.prs = append(r.prs, prRow{pr: *pr})
	}
}

func (r *connResult) addIssues(issues []forge.Issue) {
	for i := range issues {
		is := &issues[i]
		if !r.inKept(famIssues, "issue", is.Repo) || !r.firstSeen("issue", is.Repo, is.Number) {
			continue
		}
		if excluded(r.cfg, is.Author, is.Labels) {
			r.ledger.drop(famIssues, dropExcludedItem, 1)
			r.logExcluded("issue", is.Repo, is.Author, is.Labels)
			continue
		}
		r.issues = append(r.issues, issueRow{issue: *is})
	}
}

func (r *connResult) logExcluded(kind, repo, author string, labels []string) {
	r.log.Debug("excluded item", "kind", kind, "repo", bounded(repo), "author", bounded(author),
		"labels", bounded(strings.Join(labels, ",")))
}

func (c *Collector) readRuns(ctx context.Context, sc *scan, cs *connState, r *connResult) error {
	r.ledger.begin(famRuns)
	var undated []string
	dated := false
	for _, repo := range r.sortedRepos() {
		list, err := cs.conn.RunsSince(ctx, repo, sc.cutoff)
		if errors.Is(err, forge.ErrRunsUndated) {
			undated = append(undated, repo.Path)
			continue
		}
		if err != nil && (r.stopped(err, famRuns) || isShutdown(ctx)) {
			return errStopped
		}
		// A row the store cannot hold fails the whole listing: dropping it
		// alone would leave a smaller set read as complete.
		if err == nil {
			err = unstorable(&list)
		}
		if err != nil {
			if r.ledger.record(famRuns, 0, err) == outcomeFailed {
				r.warnRead("runs listing failed", "repo", repo.Path, err, hintRunsRefused)
			}
			continue
		}
		dated = dated || !list.Oldest.IsZero()
		c.listedRuns(sc, cs, r, repo, &list)
	}
	c.settleUndated(r, undated, dated)
	r.ledger.finish(famRuns)
	cs.unmapped = r.unmapped
	return nil
}

// runListing is one repository's runs as listed: every run created from
// cover on is in it, cover being the window's start, or for a listing cut
// short its oldest row. pending is the creation time of the oldest run it
// returned still pending.
type runListing struct {
	cover   time.Time
	pending time.Time
	repo    forge.Repo
	runs    []forge.Run
	cut     bool
}

// workflowListing is one repository's workflow definitions; whole when the
// listing was read to its end.
type workflowListing struct {
	repo  forge.Repo
	defs  []forge.Workflow
	whole bool
}

// listedRuns records one repository's listing of the window. A run in a
// state forgeapi could not map is reported at WARN the first scan it is seen,
// at DEBUG while it stays.
func (*Collector) listedRuns(sc *scan, cs *connState, r *connResult, repo forge.Repo, list *forge.RunList) {
	l := runListing{cover: sc.cutoff, pending: list.OldestPending, repo: repo, runs: list.Rows}
	if r.ledger.record(famRuns, list.End, nil) == outcomePartial {
		l.cut, l.cover = true, list.Oldest
		if l.cover.IsZero() {
			l.cover = sc.start
		}
		r.log.Warn("runs listing truncated", "repo", bounded(repo.Path))
	}
	r.coverageGap(sc, &l, sc.state.LastListed(cs.cfg.Name, repo.Path), sc.state.Pending(cs.cfg.Name, repo.Path))
	if n := len(list.Unmapped); n > 0 {
		level := slog.LevelDebug
		for i := range list.Unmapped {
			key := repo.Path + "#" + strconv.FormatInt(list.Unmapped[i].ID, 10)
			r.unmapped[key] = true
			if !cs.unmapped[key] {
				level = slog.LevelWarn
			}
		}
		r.log.Log(context.Background(), level, "run state unknown", "repo", bounded(repo.Path), "runs", n)
	}
	r.runLists = append(r.runLists, l)
}

// coverageGap reports, once, runs l may never list: those created since the
// previous listing prev when it is older than the window, or else a run the
// previous listing returned pending at pending that l, cut short, does not
// reach. The next listing follows this one, so neither is reported again.
func (r *connResult) coverageGap(sc *scan, l *runListing, prev, pending time.Time) {
	switch {
	case !prev.IsZero() && !runstate.Covered(prev, sc.cutoff):
		r.log.Warn("runs coverage gap", "repo", bounded(l.repo.Path), "last_listed", stamp(prev), "hours", hoursUp(sc.cutoff.Sub(prev)))
	case runstate.Stranded(l.cut, l.cover, pending):
		r.log.Warn("runs coverage gap", "repo", bounded(l.repo.Path), "last_listed", stamp(prev), "pending_since", stamp(pending),
			"hours", hoursUp(l.cover.Sub(pending)))
	}
}

// hoursUp is d in whole hours, rounded up.
func hoursUp(d time.Duration) int64 { return int64((d + time.Hour - 1) / time.Hour) }

// unstorable is why the store cannot hold one of list's completed runs, or
// why an unmapped one names no run, nil when it can hold them all.
func unstorable(list *forge.RunList) error {
	for i := range list.Rows {
		if err := runstate.Storable(&list.Rows[i]); err != nil {
			return err
		}
	}
	for i := range list.Unmapped {
		if u := &list.Unmapped[i]; u.ID <= 0 || u.CreatedAt.IsZero() {
			return fmt.Errorf("unmapped run %d has no id or creation time", u.ID)
		}
	}
	return nil
}

// settleUndated reads a product whose run rows carry no creation time, with
// no listing of the scan holding a dated row, as one without the run
// signals: an empty listing dates nothing. The undated listings, by path,
// beside a dated row are failed reads.
func (*Collector) settleUndated(r *connResult, undated []string, dated bool) {
	switch {
	case len(undated) == 0:
	case !dated:
		r.ledger.unsupport(famRuns)
		r.ledger.unsupport(famFailing)
		r.log.Warn("run listing carries no creation time; runs not read")
	default:
		for _, path := range undated {
			if r.ledger.record(famRuns, 0, forge.ErrRunsUndated) == outcomeFailed {
				r.warnRead("runs listing failed", "repo", path, forge.ErrRunsUndated, "")
			}
		}
	}
}

// listings is the scan's run listings as the store records them, each run
// marked whether it is of its repository's default branch.
func (r *connResult) listings() []runstate.Listing {
	out := make([]runstate.Listing, 0, len(r.runLists))
	for i := range r.runLists {
		l := &r.runLists[i]
		obs := make([]runstate.Observation, 0, len(l.runs))
		for j := range l.runs {
			run := &l.runs[j]
			obs = append(obs, runstate.Observation{Run: *run, OnDefault: onDefault(l.repo, run.Branch, run.Trigger)})
		}
		out = append(out, runstate.Listing{Cover: l.cover, Pending: l.pending, Repo: l.repo.Path, Runs: obs, Cut: l.cut})
	}
	return out
}

// emitRuns logs a ci run line for each new run and each changed state of the
// connection's save through out, counts those logged, and reports whether
// out logged every one.
func (r *connResult) emitRuns(out emitter, changes []runstate.Change) bool {
	for i := range changes {
		ch := &changes[i]
		if !emitRun(out, r.log, &ch.Run, ch.Previous, ch.OnDefault) {
			return false
		}
		if ch.New {
			r.sum.newRuns++
		} else {
			r.sum.changedRuns++
		}
		if ch.Run.State == forge.RunFailing {
			r.sum.newFailures++
		}
	}
	return true
}

func (c *Collector) readGitHub(ctx context.Context, _ *scan, cs *connState, r *connResult) error {
	if r.product != forge.ProductGitHub {
		return nil
	}
	r.ledger.begin(famWorkflows)
	if cs.cfg.Security.Enabled {
		r.ledger.begin(famSecurity)
	}
	for _, repo := range r.sortedRepos() {
		if err := c.readAlerts(ctx, cs, r, repo); err != nil {
			return err
		}
		if err := c.readWorkflows(ctx, cs, r, repo); err != nil {
			return err
		}
	}
	r.ledger.finish(famSecurity)
	r.ledger.finish(famWorkflows)
	return nil
}

func (*Collector) readAlerts(ctx context.Context, cs *connState, r *connResult, repo forge.Repo) error {
	sec := &cs.cfg.Security
	if !sec.Enabled {
		return nil
	}
	if (sec.SkipForks && repo.Fork) || (!sec.IncludePrivate && repo.Private) || sec.SkipRepos[strings.ToLower(repo.Path)] {
		r.log.Debug("skipping code scanning for repo", "repo", bounded(repo.Path), "fork", repo.Fork, "private", repo.Private)
		r.ledger.drop(famSecurity, dropSecuritySkipped, 1)
		return nil
	}
	r.phase = phaseSecurity
	alerts, err := cs.gh.CodeScanningAlerts(ctx, repo)
	if err != nil && (r.stopped(err, famSecurity) || isShutdown(ctx)) {
		return errStopped
	}
	switch r.ledger.record(famSecurity, alerts.End, err) {
	case outcomeFailed:
		r.warnRead("code scanning unreadable", "repo", repo.Path, err, hintSecurityRefused)
	case outcomePartial:
		r.log.Warn("code scanning listing truncated", "repo", bounded(repo.Path))
	}
	r.alerts = append(r.alerts, alerts.Rows...)
	return nil
}

func (*Collector) readWorkflows(ctx context.Context, cs *connState, r *connResult, repo forge.Repo) error {
	if repo.Fork {
		r.ledger.drop(famWorkflows, dropForkWorkflows, 1)
		return nil
	}
	r.phase = phaseWorkflows
	wfs, err := cs.gh.Workflows(ctx, repo)
	if err != nil && (r.stopped(err, famWorkflows) || isShutdown(ctx)) {
		return errStopped
	}
	switch r.ledger.record(famWorkflows, wfs.End, err) {
	case outcomeFailed:
		r.warnRead("workflows listing failed", "repo", repo.Path, err, hintWorkflowsRefused)
	case outcomeRefused:
		// Unsent: its empty listing would read as a repository with no workflows.
	default:
		r.workflowLists = append(r.workflowLists, workflowListing{repo: repo, defs: wfs.Rows, whole: wfs.End == forge.EndWhole})
	}
	return nil
}

// refusedNote opens every refusal hint: GitHub's 403 with no rate-limit
// header ends the connection's requests for the scan (see readLedger.refused).
const refusedNote = "GitHub refused this read with 403 and no rate-limit header, so this connection sent no further request this scan. "

// The remedies a GitHub refusal's warning names, by read.
const (
	hintRefused          = refusedNote + "Check that the token grants read access to this repository."
	hintRunsRefused      = refusedNote + "Check that the token grants Actions read on this repository."
	hintChecksRefused    = refusedNote + "Check that the token grants Commit statuses read on this repository. A fine-grained token cannot read check runs, so use a classic token with repo scope."
	hintSecurityRefused  = refusedNote + "If this repository has no GitHub Advanced Security, add it to security.skip_repos."
	hintWorkflowsRefused = refusedNote + "Check that the token grants Actions read on this repository."
)

// warnRead logs a failed read of the owner or repository name, with the
// hint a GitHub refusal carries.
func (r *connResult) warnRead(msg, scope, name string, err error, hint string) {
	r.log.Warn(msg, append([]any{scope, bounded(name), "error", errText(err)}, r.refusal(err, hint)...)...)
}

// refusal is the hint a warning about err carries when it is a GitHub
// refusal.
func (r *connResult) refusal(err error, hint string) []any {
	if !r.ledger.github || !errors.Is(err, forge.ErrForbidden) {
		return nil
	}
	return []any{"hint", hint}
}

// checksFailed records a failed checks read of pr, logged at WARN when GitHub
// refused it and at DEBUG otherwise: a checks read alone never escalates.
func (r *connResult) checksFailed(pr *forge.PullRequest, err error) {
	if r.ledger.record(famChecks, 0, err) != outcomeFailed {
		return
	}
	level, hint := slog.LevelDebug, r.refusal(err, hintChecksRefused)
	if hint != nil {
		level = slog.LevelWarn
	}
	r.log.Log(context.Background(), level, "pull request checks unreadable", append([]any{"repo", bounded(pr.Repo), "number", pr.Number, "error", errText(err)}, hint...)...)
}

// readChecks folds each open pull request's head checks, on a product whose
// pull-request rows name a head and a pull-request snapshot it publishes.
func (*Collector) readChecks(ctx context.Context, _ *scan, cs *connState, r *connResult) error {
	if r.ledger.fam[famChecks].unsupported || r.ledger.withheld(famPRs) {
		return nil
	}
	r.ledger.begin(famChecks)
	for i := range r.prs {
		row := &r.prs[i]
		if row.pr.HeadSHA == "" {
			r.ledger.drop(famChecks, dropNoHead, 1)
			continue
		}
		res, err := cs.conn.CommitChecks(ctx, &row.pr)
		if err != nil {
			if r.stopped(err, famChecks) || isShutdown(ctx) {
				return errStopped
			}
			r.checksFailed(&row.pr, err)
			continue
		}
		r.ledger.record(famChecks, res.End, nil)
		row.check = &res
	}
	r.ledger.finish(famChecks)
	return nil
}
