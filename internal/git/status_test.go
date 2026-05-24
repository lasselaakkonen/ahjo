package git

import (
	"strings"
	"testing"
)

func TestParseStatusV2_Counts(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid abc123",
		"# branch.head main",
		"# branch.ab +2 -0",
		"1 .M N... 100644 100644 100644 aaa bbb a",    // unstaged modify
		"1 A. N... 000000 100644 100644 000000 ccc b", // staged add
		"? untracked.txt",
		"u UU N... 100644 100644 100644 100644 d1 d2 d3 conflict.txt",
		"",
	}, "\n")

	c := ParseStatusV2(out)
	if c.Staged != 1 || c.Unstaged != 1 || c.Untracked != 1 || c.Unmerged != 1 || c.Ahead != 2 {
		t.Fatalf("counts = %+v; want staged1 unstaged1 untracked1 unmerged1 ahead2", c)
	}
	if !c.WorkTreeDirty() {
		t.Error("WorkTreeDirty() = false, want true")
	}
	if got := c.WorkTreeSummary(); strings.Contains(got, "unpushed") {
		t.Errorf("WorkTreeSummary() = %q, must not mention unpushed commits", got)
	}
	if got := c.Summary(); !strings.Contains(got, "unpushed") {
		t.Errorf("Summary() = %q, want it to mention unpushed commits", got)
	}
}

func TestParseStatusV2_Clean(t *testing.T) {
	c := ParseStatusV2("# branch.oid abc\n# branch.head main\n")
	if c.WorkTreeDirty() {
		t.Error("clean output reported WorkTreeDirty() = true")
	}
	if c.Summary() != "" || c.WorkTreeSummary() != "" {
		t.Errorf("clean summaries non-empty: %q / %q", c.Summary(), c.WorkTreeSummary())
	}
}

func TestParseStatusV2_AheadOnlyIsNotWorkTreeDirty(t *testing.T) {
	// Unpushed commits exist but the work tree is clean: the mirror would not
	// clobber anything, so this must read as not work-tree-dirty.
	c := ParseStatusV2("# branch.ab +3 -1\n")
	if c.WorkTreeDirty() {
		t.Error("ahead-only output reported WorkTreeDirty() = true")
	}
	if c.WorkTreeSummary() != "" {
		t.Errorf("WorkTreeSummary() = %q, want empty for ahead-only", c.WorkTreeSummary())
	}
	if c.Summary() == "" {
		t.Error("Summary() empty; want it to mention unpushed commits")
	}
}
