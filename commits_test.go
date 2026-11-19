// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// setupStack builds a pull request of three commits, each touching its
// own file, and returns the SHAs of the first snapshot's commits.
func setupStack(t *testing.T) (*testEnv, []string) {
	e := newTestEnv(t)
	f := e.f
	f.commit("initial", map[string]string{
		"one.txt":   numbered("one", 30, nil),
		"two.txt":   numbered("two", 30, nil),
		"three.txt": numbered("three", 30, nil),
		"base.txt":  numbered("base", 10, nil),
	})
	f.pushBase()
	f.git(f.work, "checkout", "-q", "-b", "feature")
	var shas []string
	for _, name := range []string{"one", "two", "three"} {
		shas = append(shas, f.commit("stack: change "+name+"\n\nChange-Id: I"+name, map[string]string{
			name + ".txt": numbered(name, 30, map[int]string{5: name + " line 5 changed"}),
		}))
	}
	f.pushHead(false)
	return e, shas
}

// commitBlocks returns, for each commit block on the page, its summary
// text and whether it is folded.
func commitBlocks(page string) []string {
	re := regexp.MustCompile(`(?s)<details class="commitblock([^"]*)" id="c-[0-9a-f]+"( open)?>\s*<summary>(.*?)</summary>`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		state := "open"
		if m[2] == "" {
			state = "folded"
		}
		out = append(out, state+": "+text(m[3]))
	}
	return out
}

func TestCommitReview(t *testing.T) {
	e, shas := setupStack(t)
	f := e.f
	blocks := commitBlocks(e.get(e.pr))
	if len(blocks) != 3 {
		t.Fatalf("got %d commit blocks, want 3: %q", len(blocks), blocks)
	}

	// Mark commit 1 reviewed as a whole, and commit 3 a file at a time.
	w := e.post(e.pr+"/x/commitreviewed?s=1&c="+shas[0], nil)
	if b := commitBlocks(w.Body.String()); len(b) != 1 || !strings.HasPrefix(b[0], "folded") || !strings.Contains(b[0], "✓ reviewed") {
		t.Errorf("after marking commit 1 reviewed, its block = %q, want folded and reviewed", b)
	}
	w = e.post(e.pr+"/x/reviewed?s=1&c="+shas[2]+"&f=three.txt&block=1", nil)
	if b := commitBlocks(w.Body.String()); len(b) != 1 || !strings.HasPrefix(b[0], "folded") {
		t.Errorf("after marking commit 3's only file reviewed, its block = %q, want folded", b)
	}

	// The base moves and the stack is rebased onto it, which gives every
	// commit a new ID, and commit 2 is changed besides.
	f.git(f.work, "checkout", "-q", "main")
	f.commit("base moves", map[string]string{"base.txt": numbered("base", 10, map[int]string{1: "base moved"})})
	f.pushBase()
	f.git(f.work, "checkout", "-q", "feature")
	f.git(f.work, "rebase", "-q", "main")
	f.git(f.work, "checkout", "-q", "HEAD~1")
	f.tick()
	writeFile(t, f.work+"/two.txt", numbered("two", 30, map[int]string{5: "two line 5 changed", 6: "two line 6 changed in review"}))
	f.git(f.work, "commit", "-q", "-a", "--amend", "--no-edit")
	f.git(f.work, "cherry-pick", "feature")
	f.git(f.work, "branch", "-f", "feature", "HEAD")
	f.git(f.work, "checkout", "-q", "feature")
	f.pushHead(true)

	blocks = commitBlocks(e.get(e.pr + "?refresh=1"))
	if len(blocks) != 3 {
		t.Fatalf("snapshot 2: got %d commit blocks, want 3: %q", len(blocks), blocks)
	}
	want := []struct {
		state, has, hasNot string
	}{
		{"folded", "✓ reviewed", "changed since"},
		{"open", "changed since snapshot 1", "✓ reviewed"},
		{"folded", "✓ reviewed", "changed since"},
	}
	for i, w := range want {
		b := blocks[i]
		if !strings.HasPrefix(b, w.state) || !strings.Contains(b, w.has) || strings.Contains(b, w.hasNot) {
			t.Errorf("snapshot 2 commit %d = %q; want %s, with %q, without %q", i+1, b, w.state, w.has, w.hasNot)
		}
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCommitDiffAndComment(t *testing.T) {
	e, shas := setupStack(t)
	f := e.f
	// The diff of one commit shows only that commit's file.
	page := e.get(e.pr + "/diff?s=1&c=" + shas[1] + "&f=two.txt")
	if !strings.Contains(page, "commit 2 of 3") || !strings.Contains(page, "two line 5 changed") {
		t.Fatalf("commit diff page lacks its header or change")
	}
	// ] goes on to the next commit's file.
	if !regexp.MustCompile(`id="nextfile" href="[^"]*c=` + shas[2]).MatchString(page) {
		t.Errorf("next file from commit 2 does not lead into commit 3")
	}
	if !strings.Contains(e.get(e.pr+"/diff?s=1&c="+shas[1]+"&f=two.txt"), `data-file="`+shas[1]+`:two.txt"`) {
		t.Errorf("diff page does not name the file by commit")
	}

	// A comment on commit 1's file goes on the same line of the head.
	w := e.post(e.pr+"/x/comment?s=1&c="+shas[0]+"&f=one.txt", url.Values{"side": {"right"}, "line": {"5"}, "body": {"about commit one"}})
	if !strings.Contains(w.Body.String(), "about commit one") {
		t.Fatalf("draft not echoed: %s", w.Body.String())
	}
	d, _ := e.s.store.Drafts(PRKey{"acme", "widget", 1})
	if len(d.Threads) != 1 || d.Threads[0].Commit != shas[0] || d.Threads[0].Anchor != "one line 5 changed" {
		t.Fatalf("draft = %+v", d.Threads[0])
	}
	// It shows on that commit's diff, and on the whole snapshot's.
	if !strings.Contains(e.get(e.pr+"/diff?s=1&c="+shas[0]+"&f=one.txt"), "about commit one") {
		t.Errorf("commit draft not on its commit's diff")
	}
	if !strings.Contains(e.get(e.pr+"/diff?s=1&f=one.txt"), "about commit one") {
		t.Errorf("commit draft not on the snapshot's diff")
	}
	// But not on another commit's diff of a file it has nothing to do with.
	if strings.Contains(e.get(e.pr+"/diff?s=1&c="+shas[1]+"&f=two.txt"), "about commit one") {
		t.Errorf("commit 1's draft shown on commit 2's diff")
	}

	e.post(e.pr+"/publish", url.Values{"event": {"COMMENT"}})
	var got *GHReviewComment
	for i := range f.comments {
		if strings.Contains(f.comments[i].Body, "about commit one") {
			got = &f.comments[i]
		}
	}
	if got == nil {
		t.Fatalf("commit comment not published")
	}
	if got.Line != 5 || got.Side != "RIGHT" || got.SubjectType != "line" || !strings.Contains(got.Body, fmt.Sprintf("On commit %s", shortRev(shas[0]))) {
		t.Errorf("published as %s line %d %s %q", got.Side, got.Line, got.SubjectType, got.Body)
	}
}
