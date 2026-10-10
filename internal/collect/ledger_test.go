package collect

import (
	"errors"
	"strings"
	"testing"

	"github.com/cplieger/github-scout/internal/forge"
)

// TestLedger_tables_name_every_reason_and_family pins the ledger's tables to
// their enums: a reason with no class, or a family with no name or parent,
// would read as the zero value, which is no member of either.
func TestLedger_tables_name_every_reason_and_family(t *testing.T) {
	if reasonClass[0] != 0 || familyNames[0] != "" || parent[0] != 0 {
		t.Errorf("the tables' zero slots = class %d, name %q, parent %d, want all unset: 0 is no reason and no family", reasonClass[0], familyNames[0], parent[0])
	}
	for r := dropArchived; r < numReasons; r++ {
		if c := reasonClass[r]; c < classExcluded || c > classBlind {
			t.Errorf("reason %d has class %d, want one of excluded, partial or blind", r, c)
		}
	}
	names := map[string]bool{}
	for f := range families {
		if familyNames[f] == "" || names[familyNames[f]] {
			t.Errorf("family %d is named %q, want a name of its own", f, familyNames[f])
		}
		names[familyNames[f]] = true
		if p := parent[f]; p < famRepos || p >= numFamilies {
			t.Errorf("family %s has parent %d, want a family", familyNames[f], p)
		}
	}
	if n := len(names); n != int(numFamilies)-1 {
		t.Errorf("families yields %d families, want %d", n, numFamilies-1)
	}
}

// TestLedger_operator_texts_are_plain_sentences holds every reason and hint
// a warning or scan degraded line carries, which the dashboard shows as
// written, to the rules for operator text: whole sentences, no semicolon or
// bracketed aside, and no claim about a 0 a v3 count never reports.
func TestLedger_operator_texts_are_plain_sentences(t *testing.T) {
	l := readLedger{unresolved: []string{"ghost", "typo"}}
	texts := []string{timeoutHint, hintRefused, hintRunsRefused, hintChecksRefused, hintSecurityRefused, hintWorkflowsRefused}
	for i := range diagnoses {
		texts = append(texts, diagnoses[i].reason(&l))
	}
	for _, s := range texts {
		if strings.ContainsAny(s, ";()") || strings.Contains(s, " 0 ") || !strings.HasSuffix(s, ".") || s[:1] != strings.ToUpper(s[:1]) {
			t.Errorf("operator text %q, want capitalised sentences ending in a period, with no semicolon, no bracket and no claim about 0", s)
		}
	}
}

// TestLedger_a_read_whose_source_set_no_end_fails holds the
// zero End to what it is: a source that said nothing about how its read
// ended, never a whole read.
func TestLedger_a_read_whose_source_set_no_end_fails(t *testing.T) {
	var l readLedger
	if got := l.record(famRuns, 0, nil); got != outcomeFailed || l.fam[famRuns].ok != 0 || l.fam[famRuns].drops[dropFailed] != 1 {
		t.Errorf("record(no End, no error) = %v with %d ok, %d failed, want a failed read", got, l.fam[famRuns].ok, l.fam[famRuns].drops[dropFailed])
	}
	for end, want := range map[forge.End]outcome{forge.EndWhole: outcomeOK, forge.EndCut: outcomePartial, forge.EndNone: outcomeNoData} {
		var l readLedger
		if got := l.record(famSecurity, end, nil); got != want {
			t.Errorf("record(%v) = %v, want %v", end, got, want)
		}
	}
}

func TestLedger_a_forbidden_read_refuses_only_on_github(t *testing.T) {
	for _, p := range []forge.Product{forge.ProductGitHub, forge.ProductGitLab, forge.ProductGitea} {
		var l readLedger
		l.classify(p, true)
		l.record(famRuns, 0, forge.ErrForbidden)
		if l.refused != (p == forge.ProductGitHub) {
			t.Errorf("a 403 on %s refused = %t, want %t: the quota meter that stops a scan is GitHub's", p, l.refused, p == forge.ProductGitHub)
		}
	}
	var l readLedger
	if got := l.record(famRuns, 0, errors.Join(errors.New("listing"), forge.ErrRefused)); got != outcomeRefused || l.fam[famRuns].drops[dropRefused] != 1 || !l.blind(famRuns) {
		t.Errorf("record(a read the meter held back) = %v with %d refused, blind %t, want refused, 1 and blind: an unsent read is a failed one",
			got, l.fam[famRuns].drops[dropRefused], l.blind(famRuns))
	}
}
