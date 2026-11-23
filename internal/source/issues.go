package source

import (
	"fmt"
	"sort"

	"github.com/davidhinkes/hobbs/internal/provenance"
)

// MaxRefs caps how many record IDs one issue lists.
const MaxRefs = 20

// Notes collects the problems found while parsing one record. Adapters attach
// them to the record as parse_issues; a record with notes is kept, never dropped.
type Notes struct{ Issues []string }

// Add records one problem.
func (n *Notes) Add(format string, a ...any) { n.Issues = append(n.Issues, fmt.Sprintf(format, a...)) }

// Unique returns the distinct IDs, sorted.
func Unique(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// AppendIssue appends one snapshot-level issue covering the given record IDs,
// listing at most MaxRefs of them and saying so when it lists fewer. With no
// IDs it appends nothing. msg is the caller's complete message.
func AppendIssue(issues []provenance.Issue, src string, code provenance.Code, sev provenance.Severity, ids []string, msg string) []provenance.Issue {
	if len(ids) == 0 {
		return issues
	}
	refs := Unique(ids)
	if len(refs) > MaxRefs {
		msg += fmt.Sprintf(" (first %d of %d listed)", MaxRefs, len(refs))
		refs = refs[:MaxRefs]
	}
	return append(issues, provenance.Issue{Code: code, Severity: sev, Source: src, Message: msg, Refs: refs})
}

// Truncate shortens s to at most n bytes for use in an error message.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
