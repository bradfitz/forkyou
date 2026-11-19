// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// numbered returns n lines "<prefix>1" through "<prefix>n", with the
// lines named in edits replaced.
func numbered(prefix string, n int, edits map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if e, ok := edits[i]; ok {
			b.WriteString(e + "\n")
			continue
		}
		fmt.Fprintf(&b, "%s%d\n", prefix, i)
	}
	return b.String()
}

type testEnv struct {
	t  *testing.T
	f  *fakeGitHub
	s  *server
	pr string // the pull request's URL path
}

func newTestEnv(t *testing.T) *testEnv {
	f := newFakeGitHub(t)
	gh := &GitHub{API: f.srv.URL, GraphQL: f.srv.URL + "/graphql", Token: "test-token"}
	dir := t.TempDir()
	s := newServer(gh, &Store{Dir: filepath.Join(dir, "state")}, filepath.Join(dir, "git"),
		"file://"+filepath.Join(f.dir, "origin"))
	s.viewer = "bob"
	return &testEnv{t: t, f: f, s: s, pr: "/acme/widget/pull/1"}
}

// do sends a request to the server under test and returns the response.
func (e *testEnv) do(method, target string, form url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	var req *http.Request
	if method == "POST" {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Header.Set("X-Fragment", "1")
	w := httptest.NewRecorder()
	e.s.ServeHTTP(w, req)
	return w
}

// get fetches a page, failing the test unless it is a 200.
func (e *testEnv) get(target string) string {
	e.t.Helper()
	w := e.do("GET", target, nil)
	if w.Code != 200 {
		e.t.Fatalf("GET %s: %d %s", target, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func (e *testEnv) post(target string, form url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	w := e.do("POST", target, form)
	if w.Code != 200 && w.Code != 303 && w.Code != 204 {
		e.t.Fatalf("POST %s: %d %s", target, w.Code, w.Body.String())
	}
	return w
}

// setupRebase builds a pull request with two snapshots, the second of
// which is rebased onto a base branch that has moved:
//
//	snapshot 1: a.txt line 10 changed, c.txt added
//	main moves: a.txt line 40 and b.txt line 5 changed
//	snapshot 2: rebased onto main, and a.txt line 12 changed too
func setupRebase(t *testing.T) (*testEnv, string, string) {
	e := newTestEnv(t)
	f := e.f
	f.commit("initial", map[string]string{
		"a.txt": numbered("a", 60, nil),
		"b.txt": numbered("b", 20, nil),
	})
	f.pushBase()
	main1 := f.git(f.work, "rev-parse", "HEAD")
	f.git(f.work, "checkout", "-q", "-b", "feature")
	p1 := f.commit("widget: improve", map[string]string{
		"a.txt": numbered("a", 60, map[int]string{10: "a10 changed by the PR"}),
		"c.txt": "new file\n",
	})
	f.pushHead(false)

	f.git(f.work, "checkout", "-q", main1)
	f.commit("main moves on", map[string]string{
		"a.txt": numbered("a", 60, map[int]string{40: "a40 changed on main"}),
		"b.txt": numbered("b", 20, map[int]string{5: "b5 changed on main"}),
	})
	f.pushBase()
	p2 := f.commit("widget: improve", map[string]string{
		"a.txt": numbered("a", 60, map[int]string{10: "a10 changed by the PR", 12: "a12 changed in review", 40: "a40 changed on main"}),
		"c.txt": "new file\n",
	})
	f.pushHead(true)
	return e, p1, p2
}

var tagRE = regexp.MustCompile(`<[^>]+>`)

// text strips the tags from HTML, roughly, for matching against.
func text(html string) string {
	return strings.Join(strings.Fields(tagRE.ReplaceAllString(html, " ")), " ")
}

func TestSnapshotsAndRebase(t *testing.T) {
	e, p1, p2 := setupRebase(t)

	page := e.get(e.pr)
	if !strings.Contains(page, shortRev(p1)) || !strings.Contains(page, shortRev(p2)) {
		t.Fatalf("PR page lacks the snapshots' commits")
	}
	if !strings.Contains(text(page), "rebased onto") {
		t.Errorf("PR page does not say snapshot 2 was rebased")
	}
	if !strings.Contains(text(page), "force push") {
		t.Errorf("PR page does not say snapshot 2 was force-pushed")
	}

	// The whole change at snapshot 2: a.txt and c.txt, not b.txt.
	files := e.get(e.pr + "?base=parent&s=2")
	if !strings.Contains(files, `data-file="a.txt"`) || !strings.Contains(files, `data-file="c.txt"`) {
		t.Errorf("snapshot 2 file list lacks a.txt or c.txt")
	}
	if strings.Contains(files, `data-file="b.txt"`) {
		t.Errorf("snapshot 2 against its parent lists b.txt, which only main changed")
	}

	// Snapshot 1 against 2: a.txt is the PR's own work and counts only
	// the line it changed in review. That counts as an addition only: the
	// old side of the row is the base's line, which the PR did not write
	// (see MarkRebased). b.txt is listed but held back as
	// rebase-only; c.txt did not change at all.
	cmp := e.get(e.pr + "?base=1&s=2")
	row := regexp.MustCompile(`(?s)<tr class="item filerow( rebaseonly)?"[^>]*data-file="([^"]+)".*?</tr>`)
	got := map[string]string{}
	for _, m := range row.FindAllStringSubmatch(cmp, -1) {
		got[m[2]] = text(m[0])
		if m[1] != "" {
			got[m[2]] += " HIDDEN"
		}
	}
	if !strings.Contains(got["a.txt"], "+1 −0") || strings.Contains(got["a.txt"], "HIDDEN") {
		t.Errorf("a.txt row = %q, want +1 −0 and shown", got["a.txt"])
	}
	if !strings.Contains(got["b.txt"], "HIDDEN") {
		t.Errorf("b.txt row = %q, want hidden as rebase-only", got["b.txt"])
	}
	if _, ok := got["c.txt"]; ok {
		t.Errorf("c.txt listed in 1→2 comparison though it did not change")
	}

	// In the diff, line 40 is muted as rebased and line 12 is not.
	diff := e.get(e.pr + "/diff?f=a.txt&base=1&s=2")
	rowFor := func(l int) string {
		m := regexp.MustCompile(fmt.Sprintf(`<tr class="diffrow ([^"]*)" data-l="%d"`, l)).FindStringSubmatch(diff)
		if m == nil {
			t.Fatalf("no diff row for line %d", l)
		}
		return m[1]
	}
	if c := rowFor(40); !strings.Contains(c, "rebased") {
		t.Errorf("line 40 row class = %q, want rebased", c)
	}
	if c := rowFor(12); strings.Contains(c, "rebased") || !strings.Contains(c, "replace") {
		t.Errorf("line 12 row class = %q, want an own replace", c)
	}
	if !strings.Contains(diff, "Muted lines came along with a rebase") {
		t.Errorf("diff does not explain the muted lines")
	}

	// range-diff pairs the commit with its new version.
	rd := e.get(e.pr + "/rangediff?base=1&s=2")
	if !strings.Contains(rd, "rdchanged") {
		t.Errorf("range-diff did not pair the two versions of the commit:\n%s", text(rd))
	}
}

func TestCommentAndPublish(t *testing.T) {
	e, p1, _ := setupRebase(t)
	f := e.f
	// Someone has already commented on snapshot 1.
	carol := f.addComment(GHReviewComment{
		Path: "a.txt", Line: 10, OriginalLine: 10, Side: "RIGHT",
		CommitID: p1, OriginalCommitID: p1, Body: "why this?", User: GHUser{Login: "carol"},
	})
	thread := fmt.Sprintf("g%d", carol.ID)

	// It shows on the left when comparing snapshot 1 with 2, and on the
	// right, marked as from snapshot 1, on snapshot 2 alone.
	diff := e.get(e.pr + "/diff?f=a.txt&base=1&s=2")
	if !strings.Contains(diff, "why this?") {
		t.Fatalf("carol's comment not on the diff")
	}
	diff = e.get(e.pr + "/diff?f=a.txt&base=parent&s=2")
	if !strings.Contains(diff, "why this?") || !strings.Contains(diff, "snapshot 1") {
		t.Errorf("carol's comment not carried to snapshot 2, marked as from snapshot 1")
	}

	draft := func(q string, line, start int, body string) {
		t.Helper()
		v := url.Values{"side": {"right"}, "line": {fmt.Sprint(line)}, "body": {body}}
		if start > 0 {
			v.Set("start", fmt.Sprint(start))
		}
		w := e.post(e.pr+"/x/comment?"+q, v)
		if !strings.Contains(w.Body.String(), body) {
			t.Fatalf("draft %q not echoed back: %s", body, w.Body.String())
		}
	}
	draft("f=a.txt&base=parent&s=2", 12, 0, "nice line 12")      // in GitHub's diff
	draft("f=a.txt&base=parent&s=1", 10, 0, "about snapshot one") // carried over to snapshot 2
	draft("f=a.txt&base=parent&s=2", 30, 0, "far from the diff")  // outside every hunk
	draft("f=a.txt&base=parent&s=2", 11, 9, "a range")            // lines 9-11
	draft("f=b.txt&base=parent&s=2", 3, 0, "b is not in the PR")  // not in the PR at all
	e.post(e.pr+"/x/comment?thread="+thread, url.Values{"body": {"because"}, "resolve": {"1"}})

	page := e.get(e.pr)
	if !strings.Contains(text(page), "Reply (6 drafts)") {
		t.Errorf("PR page does not count 6 drafts")
	}

	w := e.post(e.pr+"/publish", url.Values{"event": {"COMMENT"}, "message": {"Overall fine."}})
	if loc := w.Header().Get("Location"); strings.Contains(loc, "flash") {
		t.Fatalf("publish failed: %s", loc)
	}

	d, _ := e.s.store.Drafts(PRKey{"acme", "widget", 1})
	if d.Count() != 0 {
		t.Errorf("%d drafts left after publishing", d.Count())
	}
	byBody := map[string]GHReviewComment{}
	for _, c := range f.comments {
		for _, key := range []string{"nice line 12", "about snapshot one", "far from the diff", "a range", "because"} {
			if strings.Contains(c.Body, key) {
				byBody[key] = c
			}
		}
	}
	check := func(key string, line, start int, subject string) {
		t.Helper()
		c, ok := byBody[key]
		if !ok {
			t.Errorf("%q not published", key)
			return
		}
		if c.Line != line || c.StartLine != start || c.SubjectType != subject {
			t.Errorf("%q published at line %d start %d %s; want line %d start %d %s",
				key, c.Line, c.StartLine, c.SubjectType, line, start, subject)
		}
	}
	check("nice line 12", 12, 0, "line")
	check("about snapshot one", 10, 0, "line")
	check("far from the diff", 0, 0, "file")
	check("a range", 11, 9, "line")
	if c := byBody["far from the diff"]; !strings.Contains(c.Body, "a30") {
		t.Errorf("file comment does not quote its line: %q", c.Body)
	}
	if c := byBody["because"]; c.InReplyTo != carol.ID {
		t.Errorf("reply published with in_reply_to %d, want %d", c.InReplyTo, carol.ID)
	}
	if len(f.reviews) != 1 {
		t.Fatalf("%d reviews, want 1", len(f.reviews))
	}
	body := f.reviews[0].Body
	if !strings.Contains(body, "Overall fine.") || !strings.Contains(body, "b is not in the PR") || !strings.Contains(body, "b3") {
		t.Errorf("review body lacks the message or the quoted b.txt comment:\n%s", body)
	}
	if !f.threads[carol.ID].resolved {
		t.Errorf("carol's thread not resolved after publishing a resolving reply")
	}

	// Resolve and unresolve act on GitHub at once.
	w = e.post(e.pr+"/x/resolve?thread="+thread, nil)
	if f.threads[carol.ID].resolved {
		t.Errorf("thread still resolved after unresolving")
	}
	if !strings.Contains(w.Body.String(), "Resolve") {
		t.Errorf("resolve fragment = %s", w.Body.String())
	}
}

func TestPublishFailureKeepsDrafts(t *testing.T) {
	e, _, _ := setupRebase(t)
	e.post(e.pr+"/x/comment?f=a.txt&s=2", url.Values{"side": {"right"}, "line": {"12"}, "body": {"keep me"}})
	e.f.failGraphQL = map[string]string{"submitPullRequestReview": "something broke"}
	w := e.post(e.pr+"/publish", url.Values{"event": {"COMMENT"}, "message": {"msg"}})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "flash") {
		t.Fatalf("publish did not report failure; redirected to %s", loc)
	}
	d, _ := e.s.store.Drafts(PRKey{"acme", "widget", 1})
	if d.Count() != 1 || d.Message != "msg" {
		t.Errorf("after failure: %d drafts, message %q; want 1 and %q", d.Count(), d.Message, "msg")
	}
	if e.f.pending != nil {
		t.Errorf("failed publish left a pending review on GitHub")
	}
}

func TestDraftEditDelete(t *testing.T) {
	e, _, _ := setupRebase(t)
	e.post(e.pr+"/x/comment?f=a.txt&s=2", url.Values{"side": {"left"}, "line": {"5"}, "body": {"left side"}})
	d, _ := e.s.store.Drafts(PRKey{"acme", "widget", 1})
	if len(d.Threads) != 1 {
		t.Fatalf("%d draft threads", len(d.Threads))
	}
	dt := d.Threads[0]
	if dt.Side != "old" || dt.Anchor != "a5" {
		t.Errorf("left-side draft on snapshot 2 = side %q anchor %q; want old, a5", dt.Side, dt.Anchor)
	}
	w := e.post(e.pr+"/x/edit?comment="+dt.ID, url.Values{"body": {"edited"}})
	if !strings.Contains(w.Body.String(), "edited") {
		t.Errorf("edit fragment lacks new text")
	}
	w = e.post(e.pr+"/x/delete?comment="+dt.ID, nil)
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Errorf("deleting the only draft in a thread returned %q, want nothing", w.Body.String())
	}
	d, _ = e.s.store.Drafts(PRKey{"acme", "widget", 1})
	if d.Count() != 0 {
		t.Errorf("%d drafts after delete", d.Count())
	}
}

func TestReviewedMarks(t *testing.T) {
	e, _, _ := setupRebase(t)
	// Marking snapshot 1 reviewed makes snapshot 2's page default to
	// showing what changed since.
	e.post(e.pr+"/x/snapreviewed?n=1", nil)
	page := e.get(e.pr)
	if !strings.Contains(page, "showing changes since you last reviewed") {
		t.Errorf("after marking snapshot 1 reviewed, the PR page does not default to comparing with it")
	}
	// c.txt did not change between the snapshots, so its mark carries.
	diff := e.get(e.pr + "/diff?f=c.txt&base=parent")
	if !strings.Contains(diff, "✓ reviewed") {
		t.Errorf("c.txt mark did not carry from snapshot 1 to 2")
	}
	// a.txt did change, so it starts unreviewed.
	diff = e.get(e.pr + "/diff?f=a.txt&base=parent")
	if strings.Contains(diff, "✓ reviewed") {
		t.Errorf("a.txt mark carried though the file changed")
	}
	w := e.post(e.pr+"/x/reviewed?f=a.txt&s=2", nil)
	if !strings.Contains(w.Body.String(), "✓ reviewed") {
		t.Errorf("toggle reviewed = %s", w.Body.String())
	}
}

func TestGoURL(t *testing.T) {
	e := newTestEnv(t)
	for in, want := range map[string]string{
		"/go?u=https://github.com/acme/widget/pull/7":          "/acme/widget/pull/7",
		"/go?u=acme/widget/pull/7/files":                      "/acme/widget/pull/7",
		"/github.com/acme/widget/pull/7":                      "/acme/widget/pull/7",
		"/go?u=" + url.QueryEscape("github.com/acme/widget"): "/acme/widget",
	} {
		w := e.do("GET", in, nil)
		if w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d %s, want redirect to %s", in, w.Code, w.Header().Get("Location"), want)
		}
	}
}
