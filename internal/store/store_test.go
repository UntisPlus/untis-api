package store

import (
	"path/filepath"
	"testing"
)

func TestReplaceClassSnapshotDiff(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := st.ClassVersion("s", 1); got != 0 {
		t.Fatalf("initial version = %d, want 0", got)
	}

	// initial snapshot: 2 periods
	v1 := []PeriodRow{
		{PeriodID: 100, Start: "a1", End: "b1", Subject: "S1"},
		{PeriodID: 200, Start: "a2", End: "b2", Subject: "S2"},
	}
	changed, err := st.ReplaceClassSnapshot("s", 1, v1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Fatalf("first write changed=%d, want 2", changed)
	}
	if got := st.ClassVersion("s", 1); got != 1 {
		t.Fatalf("version after first write = %d, want 1", got)
	}

	// unchanged snapshot: no change
	changed, err = st.ReplaceClassSnapshot("s", 1, v1, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("unchanged write changed=%d, want 0", changed)
	}
	if got := st.ClassVersion("s", 1); got != 1 {
		t.Fatalf("version after unchanged write = %d, want 1", got)
	}

	// one CHANGED, one REMOVED, one ADDED
	v2 := []PeriodRow{
		{PeriodID: 100, Start: "a1", End: "b1", Subject: "S1-NEW"}, // changed
		{PeriodID: 300, Start: "a3", End: "b3", Subject: "S3"},      // added
	}
	changed, err = st.ReplaceClassSnapshot("s", 1, v2, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 3 {
		t.Fatalf("diff write changed=%d, want 3 (1 changed + 1 added + 1 removed)", changed)
	}
	if got := st.ClassVersion("s", 1); got != 2 {
		t.Fatalf("version after diff write = %d, want 2", got)
	}

	pending, cur, err := st.PendingChanges("s", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cur != 2 {
		t.Fatalf("pending current = %d, want 2", cur)
	}
	kinds := map[int64]string{}
	for _, r := range pending {
		kinds[r.PeriodID] = r.Kind
	}
	if kinds[100] != "CHANGED" {
		t.Fatalf("period 100 kind = %q, want CHANGED", kinds[100])
	}
	if kinds[300] != "ADDED" {
		t.Fatalf("period 300 kind = %q, want ADDED", kinds[300])
	}
	if _, ok := kinds[200]; !ok {
		t.Fatalf("period 200 (removed) missing from pending: %+v", kinds)
	}
	// after consuming up to 2, nothing pending
	pending2, cur2, err := st.PendingChanges("s", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if cur2 != 2 {
		t.Fatalf("current after consume = %d, want 2", cur2)
	}
	if len(pending2) != 0 {
		t.Fatalf("pending after consume = %d, want 0", len(pending2))
	}
}

func TestDropRemovedBefore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// snapshot with one past period (started yesterday) and one today
	v1 := []PeriodRow{
		{PeriodID: 1, Start: "2026-08-28T10:00Z", End: "2026-08-28T10:45Z", Subject: "A"},
		{PeriodID: 2, Start: "2026-08-29T10:00Z", End: "2026-08-29T10:45Z", Subject: "B"},
	}
	if _, err := st.ReplaceClassSnapshot("s", 2, v1, 1, ""); err != nil {
		t.Fatal(err)
	}
	// next poll: no periods (both left the window); drop before today (2026-08-29).
	// Period 1 (2026-08-28) should silently disappear; period 2 (2026-08-29) removed+reported.
	changed, err := st.ReplaceClassSnapshot("s", 2, nil, 2, "2026-08-29")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1 (only the today period reported as removed)", changed)
	}
	pending, _, _ := st.PendingChanges("s", 2, 1)
	if len(pending) != 1 || pending[0].PeriodID != 2 || pending[0].Kind != "REMOVED" {
		t.Fatalf("pending = %+v, want only period 2 REMOVED", pending)
	}
}
