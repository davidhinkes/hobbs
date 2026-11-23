package source

import (
	"fmt"
	"strings"
	"testing"

	"github.com/davidhinkes/hobbs/internal/provenance"
)

func TestAppendIssue(t *testing.T) {
	if got := AppendIssue(nil, "s", provenance.ParseFailed, provenance.Warning, nil, "m"); len(got) != 0 {
		t.Error("no IDs, no issue")
	}
	got := AppendIssue(nil, "s", provenance.ParseFailed, provenance.Warning, []string{"b", "a", "b"}, "2 records")
	if len(got) != 1 || strings.Join(got[0].Refs, ",") != "a,b" || got[0].Message != "2 records" || got[0].Source != "s" {
		t.Errorf("got %+v", got)
	}
	var many []string
	for i := 0; i < 50; i++ {
		many = append(many, fmt.Sprintf("id%02d", i))
	}
	got = AppendIssue(got, "s", provenance.GeometryUnavailable, provenance.Warning, many, "50 records")
	if len(got) != 2 || len(got[1].Refs) != MaxRefs || !strings.HasSuffix(got[1].Message, "(first 20 of 50 listed)") {
		t.Errorf("cap: %+v", got[1])
	}
}

func TestNotesAndTruncate(t *testing.T) {
	var n Notes
	n.Add("a %d", 1)
	n.Add("b")
	if strings.Join(n.Issues, ",") != "a 1,b" {
		t.Errorf("notes = %v", n.Issues)
	}
	if Truncate("abcdef", 3) != "abc..." || Truncate("ab", 3) != "ab" {
		t.Error("truncate")
	}
}
