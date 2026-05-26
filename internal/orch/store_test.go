package orch

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	tmp, err := os.MkdirTemp("", "orch-store-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	path := tmp + "/state.db"

	// First open: empty.
	store, err := openStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	jobs, cur, maint, err := store.LoadState()
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(jobs) != 0 || cur != nil || maint != nil {
		t.Fatalf("expected empty state, got jobs=%v cur=%v maint=%v", jobs, cur, maint)
	}

	// Save some data.
	cursor := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	in := map[int]*Job{
		1: {VM: "local", Tmux: "claude-1", Target: "deno", TargetRepo: "denoland/deno", Branch: "orch/issue-1", PR: 42},
		7: {VM: "remote", Tmux: "claude-7", Target: "orchid", TargetRepo: "denoland/orchid", Branch: "orch/issue-7", SeenReviewIDs: []string{"r1", "r2"}},
	}
	m := &MaintainerCache{FetchedAt: cursor, Members: []string{"alice", "bob"}}
	if err := store.SaveState(in, &cursor, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.PutSnap([]byte(`{"layout":1}`)); err != nil {
		t.Fatalf("putSnap: %v", err)
	}
	// Rotate via second snap.
	if err := store.PutSnap([]byte(`{"layout":2}`)); err != nil {
		t.Fatalf("putSnap v2: %v", err)
	}
	store.Close()

	// Reopen and verify.
	store2, err := openStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	out, cur2, maint2, err := store2.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(out))
	}
	if !reflect.DeepEqual(out[1].SeenReviewIDs, in[1].SeenReviewIDs) {
		t.Errorf("job 1 mismatch: %#v vs %#v", out[1], in[1])
	}
	if out[7].PR != 0 || out[7].Tmux != "claude-7" {
		t.Errorf("job 7 wrong: %#v", out[7])
	}
	if cur2 == nil || !cur2.Equal(cursor) {
		t.Errorf("cursor: got %v want %v", cur2, cursor)
	}
	if maint2 == nil || len(maint2.Members) != 2 || maint2.Members[0] != "alice" {
		t.Errorf("maintainers: %#v", maint2)
	}
	snap, err := store2.GetSnap()
	if err != nil {
		t.Fatalf("getSnap: %v", err)
	}
	if string(snap) != `{"layout":2}` {
		t.Errorf("snap: got %q want layout v2", snap)
	}
	v, _, _ := store2.getKV("snap.bak")
	if string(v) != `{"layout":1}` {
		t.Errorf("snap.bak: got %q want layout v1", v)
	}
}

func TestStoreMigrateLegacyJSON(t *testing.T) {
	tmp, err := os.MkdirTemp("", "orch-migr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	stateJSON := tmp + "/state.json"
	snapJSON := tmp + "/snap.json"
	dbPath := tmp + "/state.db"

	// Legacy state.json with one job and a cursor.
	legacy := `{"jobs":{"5":{"vm":"local","tmux":"claude-5","branch":"orch/issue-5","pr":99}},"mention_cursor":"2026-05-25T10:00:00Z"}`
	if err := os.WriteFile(stateJSON, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapJSON, []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := openStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrateLegacyJSON(stateJSON, snapJSON); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	jobs, cur, _, err := store.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(jobs) != 1 || jobs[5] == nil || jobs[5].PR != 99 {
		t.Fatalf("migrated jobs: %#v", jobs)
	}
	if cur == nil {
		t.Fatalf("cursor not migrated")
	}
	if _, err := os.Stat(stateJSON); !os.IsNotExist(err) {
		t.Errorf("legacy state.json was not renamed: %v", err)
	}
	if _, err := os.Stat(stateJSON + ".migrated"); err != nil {
		t.Errorf("migrated marker missing: %v", err)
	}
	snap, _ := store.GetSnap()
	if string(snap) != `{"old":true}` {
		t.Errorf("snap not migrated: %q", snap)
	}
	store.Close()
}
