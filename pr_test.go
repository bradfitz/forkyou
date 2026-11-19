// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func heads(snaps []*Snapshot) []string {
	var out []string
	for _, s := range snaps {
		out = append(out, shortRev(s.Head)[:8])
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBuildSnapshotsReal reconstructs the snapshots of a real pull request
// that was force-pushed four times. Its first head appears in the REST
// timeline only as the commit a review was written against.
func TestBuildSnapshotsReal(t *testing.T) {
	var pull GHPull
	var tl []GHTimelineEvent
	var reviews []GHReview
	loadJSON(t, "testdata/pulls_22447.json", &pull)
	loadJSON(t, "testdata/tl22447.json", &tl)
	loadJSON(t, "testdata/pulls_22447_reviews.json", &reviews)

	snaps := buildSnapshots(&pull, tl, reviews, nil)
	want := []string{"d940f5ba", "044ea804", "6fb12097", "69175fdb", "48a54e66"}
	if got := heads(snaps); !equalStrings(got, want) {
		t.Fatalf("snapshots = %v, want %v", got, want)
	}
	for i, s := range snaps {
		if s.N != i+1 {
			t.Errorf("snapshot %d numbered %d", i+1, s.N)
		}
	}
	if !snaps[1].Force || snaps[1].Pusher != "kari-ts" {
		t.Errorf("snapshot 2 = force %v by %q, want a force push by kari-ts", snaps[1].Force, snaps[1].Pusher)
	}
	if snaps[0].Force {
		t.Errorf("snapshot 1 marked as a force push")
	}
}

func at(min int) time.Time {
	return time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC)
}

func committed(sha, parent string, when time.Time) GHTimelineEvent {
	e := GHTimelineEvent{Event: "committed", SHA: sha}
	e.Committer = &struct {
		Name  string    `json:"name"`
		Email string    `json:"email"`
		Date  time.Time `json:"date"`
	}{Date: when}
	if parent != "" {
		e.Parents = append(e.Parents, struct {
			SHA string `json:"sha"`
		}{parent})
	}
	return e
}

func forced(sha string, when time.Time) GHTimelineEvent {
	return GHTimelineEvent{Event: "head_ref_force_pushed", CommitID: sha, CreatedAt: &when, Actor: &GHUser{Login: "pusher"}}
}

func TestBuildSnapshotsCoalesce(t *testing.T) {
	// Three commits made a few seconds apart are one push. A fourth an
	// hour later is another.
	tl := []GHTimelineEvent{
		committed("aaaa0000", "base", at(0)),
		committed("bbbb0000", "aaaa0000", at(1)),
		committed("cccc0000", "bbbb0000", at(1)),
		committed("dddd0000", "cccc0000", at(60)),
	}
	pull := &GHPull{Head: GHRef{SHA: "dddd0000"}, UpdatedAt: at(61)}
	got := heads(buildSnapshots(pull, tl, nil, nil))
	if want := []string{"cccc0000", "dddd0000"}; !equalStrings(got, want) {
		t.Errorf("snapshots = %v, want %v", got, want)
	}

	// A review of the middle commit says it was once the head.
	reviews := []GHReview{{State: "COMMENTED", CommitID: "bbbb0000", SubmittedAt: at(2)}}
	got = heads(buildSnapshots(pull, tl, reviews, nil))
	if want := []string{"bbbb0000", "cccc0000", "dddd0000"}; !equalStrings(got, want) {
		t.Errorf("with review: snapshots = %v, want %v", got, want)
	}
}

func TestBuildSnapshotsForcePushBefore(t *testing.T) {
	// GraphQL names the head a force push replaced, which the REST
	// timeline has forgotten: its committed events are for the
	// commits on the branch now.
	tl := []GHTimelineEvent{
		committed("bbbb0000", "base", at(5)),
		forced("bbbb0000", at(6)),
	}
	fps := []GQLForcePush{{
		CreatedAt:    at(6),
		BeforeCommit: &struct{ OID string }{"aaaa0000"},
		AfterCommit:  &struct{ OID string }{"bbbb0000"},
	}}
	pull := &GHPull{Head: GHRef{SHA: "bbbb0000"}, UpdatedAt: at(7)}
	got := heads(buildSnapshots(pull, tl, nil, fps))
	if want := []string{"aaaa0000", "bbbb0000"}; !equalStrings(got, want) {
		t.Errorf("snapshots = %v, want %v", got, want)
	}
}

func TestParseHunks(t *testing.T) {
	diff := `diff --git a/a.go b/a.go
index 1111111..2222222 100644
--- a/a.go
+++ b/a.go
@@ -1,5 +1,6 @@ package a
 x
+y
 z
@@ -40 +41,2 @@ func f() {
-old
+new
+newer
diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,3 @@
+1
+2
+3
diff --git a/bin b/bin
Binary files a/bin and b/bin differ
`
	h := parseHunks(diff)
	if got := h["a.go"]; len(got) != 2 || got[0] != (hunk{1, 5, 1, 6}) || got[1] != (hunk{40, 40, 41, 42}) {
		t.Errorf("a.go hunks = %v", got)
	}
	if got := h["new.txt"]; len(got) != 1 || got[0] != (hunk{0, -1, 1, 3}) {
		t.Errorf("new.txt hunks = %v", got)
	}
	if got, ok := h["bin"]; !ok || got != nil {
		t.Errorf("bin = %v, %v; want present with no hunks", got, ok)
	}
	if !inHunk(h["a.go"], "RIGHT", 0, 6) || inHunk(h["a.go"], "RIGHT", 0, 7) {
		t.Errorf("inHunk RIGHT wrong")
	}
	if !inHunk(h["a.go"], "RIGHT", 41, 42) || inHunk(h["a.go"], "RIGHT", 6, 41) {
		t.Errorf("inHunk range wrong: a range must sit inside one hunk")
	}
	if !inHunk(h["a.go"], "LEFT", 0, 40) || inHunk(h["a.go"], "LEFT", 0, 41) {
		t.Errorf("inHunk LEFT wrong")
	}
}

func TestAnchor(t *testing.T) {
	lines := []string{"a", "b", "target", "c"}
	for _, tt := range []struct {
		line   int
		anchor string
		want   int
		stale  bool
	}{
		{3, "target", 3, false},
		{1, "target", 3, false},
		{30, "target", 0, true}, // too far away to be the same line
		{2, "gone", 0, true},
		{0, "", 0, false},
		{4, "", 4, false},
	} {
		got, stale := Anchor(&Thread{Line: tt.line, Anchor: tt.anchor}, lines)
		if got != tt.want || stale != tt.stale {
			t.Errorf("Anchor(%d, %q) = %d, %v; want %d, %v", tt.line, tt.anchor, got, stale, tt.want, tt.stale)
		}
	}
}
