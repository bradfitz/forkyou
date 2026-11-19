// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseBody(t *testing.T) {
	th := &bodyMarker{Kind: "thread", ID: "abc123", Head: "h", Commit: "c1", File: commitMsgPath, Line: 3, StartLine: 2}
	rp := &bodyMarker{Kind: "reply", ID: "abc123", Resolved: true}
	body := "Overall fine.\n\n---\n\n" +
		th.String() + "\nCommit message of c1 (\"subj\"), lines 2-3:\n```\nline two\nline three\n```\n\nReword this.\nPlease.\n" + bodyEnd +
		"\n\n---\n\n" + rp.String() + "\n(Replying to bob on `x`: \"Reword this.\".)\n\nDone.\n" + bodyEnd
	secs := parseBody(body)
	if len(secs) != 2 {
		t.Fatalf("got %d sections, want 2: %+v", len(secs), secs)
	}
	s := secs[0]
	if s.Kind != "thread" || s.ID != "abc123" || s.File != commitMsgPath || s.Commit != "c1" || s.Line != 3 || s.StartLine != 2 || s.Side != "new" {
		t.Errorf("thread section = %+v", s.bodyMarker)
	}
	if s.Body != "Reword this.\nPlease." {
		t.Errorf("thread body = %q", s.Body)
	}
	if r := secs[1]; r.Kind != "reply" || r.ID != "abc123" || !r.Resolved || r.Body != "Done." {
		t.Errorf("reply section = %+v %q", r.bodyMarker, r.Body)
	}
	// A file name with spaces and odd characters survives the round trip.
	odd := &bodyMarker{Kind: "thread", ID: "x", File: "a dir/we--ird>name.go", Line: 1}
	if got := parseBody(odd.String() + "\nhi\n" + bodyEnd); len(got) != 1 || got[0].File != odd.File {
		t.Errorf("odd file name round trip = %+v", got)
	}
}

func TestCommitMsgComments(t *testing.T) {
	e, _, p2 := setupRebase(t)
	f := e.f

	// The message is the first file, a new file against the parent.
	page := e.get(e.pr)
	if i, j := strings.Index(page, `data-file="/COMMIT_MSG"`), strings.Index(page, `data-file="a.txt"`); i < 0 || i > j {
		t.Fatalf("/COMMIT_MSG not listed first")
	}
	diff := e.get(e.pr + "/diff?f=%2FCOMMIT_MSG")
	if !strings.Contains(diff, "widget: improve") {
		t.Fatalf("/COMMIT_MSG diff lacks the message")
	}

	w := e.post(e.pr+"/x/comment?f=%2FCOMMIT_MSG", url.Values{"side": {"right"}, "line": {"1"}, "body": {"Say what improved."}})
	if !strings.Contains(w.Body.String(), "Say what improved.") {
		t.Fatalf("draft not echoed: %s", w.Body.String())
	}
	d, _ := e.s.store.Drafts(PRKey{"acme", "widget", 1})
	if dt := d.Threads[0]; dt.Commit != p2 || dt.Line != 1 || dt.Anchor != "widget: improve" {
		t.Fatalf("draft = %+v", dt)
	}
	e.post(e.pr+"/publish", url.Values{"event": {"COMMENT"}, "message": {"Looks fine."}})
	if len(f.reviews) != 1 {
		t.Fatalf("%d reviews", len(f.reviews))
	}
	body := f.reviews[0].Body
	for _, want := range []string{"Looks fine.", "Commit message of " + shortRev(p2), "```\nwidget: improve\n```", "Say what improved.", "<!-- forkyou:thread "} {
		if !strings.Contains(body, want) {
			t.Errorf("review body lacks %q:\n%s", want, body)
		}
	}
	if len(f.comments) != 0 {
		t.Errorf("commit message comment also made %d line comments", len(f.comments))
	}

	// Read back, it is a thread on line 1 of the message again.
	diff = e.get(e.pr + "/diff?f=%2FCOMMIT_MSG&refresh=1")
	if !strings.Contains(diff, "Say what improved.") || !strings.Contains(diff, "in review body") {
		t.Fatalf("published commit message comment not shown as a thread")
	}
	if strings.Contains(diff, "Commit message of") {
		t.Errorf("thread shows the quote heading meant for github.com")
	}
	var id string
	e.s.mu.Lock()
	for _, th := range e.s.prs[PRKey{"acme", "widget", 1}].pr.Threads {
		if th.InBody {
			id = th.ID
		}
	}
	e.s.mu.Unlock()
	if id == "" {
		t.Fatalf("no body thread parsed")
	}

	// Replying with resolve resolves it.
	e.post(e.pr+"/x/comment?thread="+id, url.Values{"body": {"Done"}, "resolve": {"1"}})
	e.post(e.pr+"/publish", url.Values{"event": {"COMMENT"}})
	page = e.get(e.pr + "?refresh=1")
	if !strings.Contains(text(page), "1 resolved") {
		t.Errorf("after a resolving reply, the thread is not resolved")
	}

	// Reword the message, keeping the subject, and force-push.
	f.git(f.work, "commit", "-q", "--amend", "-m", "widget: improve\n\nNow it says why.")
	f.pushHead(true)
	cmp := e.get(e.pr + "/diff?f=%2FCOMMIT_MSG&base=2&s=3&refresh=1")
	if !strings.Contains(cmp, "Now it says why.") || !strings.Contains(cmp, `class="diffrow insert`) {
		t.Errorf("snapshot 2 to 3 /COMMIT_MSG diff does not show the new message")
	}
	if !strings.Contains(cmp, "Say what improved.") {
		t.Errorf("comment on the old message is not carried to the new version")
	}
	rd := e.get(e.pr + "/rangediff?base=2&s=3")
	if !strings.Contains(rd, "Commit message") || !strings.Contains(rd, "Now it says why.") {
		t.Errorf("range-diff does not show the reworded message:\n%s", text(rd))
	}
}
