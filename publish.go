// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Publishing turns the drafts into a single GitHub review, as Gerrit
// publishes a set of draft comments as one reply: one notification for
// the author instead of one per remark.
//
// GitHub is pickier than this program about where a comment may go. It
// takes a line comment only against the newest head, and only on a line
// inside one of the hunks of its own diff. Here anything can be commented
// on: any line of any file, in any snapshot. So each draft is carried over
// to the newest snapshot first, by the text of its line, and then placed
// as well as GitHub allows:
//
//   - on its line, if that line is in a hunk of GitHub's diff;
//   - otherwise as a comment on the whole file, quoting the line, if
//     GitHub's diff has the file at all;
//   - otherwise in the body of the review, quoting the file and line.
//
// Nothing written is ever dropped for having been written somewhere
// GitHub would not have allowed it.

// A hunk is the line ranges of one hunk of a diff, inclusive.
type hunk struct {
	OldStart, OldEnd int
	NewStart, NewEnd int
}

var hunkHeaderRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseHunks reads the hunks of each file out of a unified diff. A file
// in the diff with no hunks (a mode change, say, or a binary file) is
// present with a nil list.
func parseHunks(diff string) map[string][]hunk {
	m := map[string][]hunk{}
	var cur string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// The new name, which is what GitHub keys comments by. It is
			// read from the +++ line when there is one, since names with
			// spaces make this line ambiguous.
			if i := strings.LastIndex(line, " b/"); i >= 0 {
				cur = line[i+3:]
				m[cur] = nil
			}
		case strings.HasPrefix(line, "+++ "):
			name := strings.TrimPrefix(line, "+++ ")
			if name == "/dev/null" {
				continue
			}
			if n, err := strconv.Unquote(name); err == nil {
				name = n
			}
			name = strings.TrimPrefix(name, "b/")
			if name != cur {
				delete(m, cur)
				cur = name
				m[cur] = nil
			}
		case strings.HasPrefix(line, "@@ "):
			sm := hunkHeaderRE.FindStringSubmatch(line)
			if sm == nil {
				continue
			}
			num := func(s string, def int) int {
				if s == "" {
					return def
				}
				n, _ := strconv.Atoi(s)
				return n
			}
			os, ol := num(sm[1], 0), num(sm[2], 1)
			ns, nl := num(sm[3], 0), num(sm[4], 1)
			m[cur] = append(m[cur], hunk{os, os + ol - 1, ns, ns + nl - 1})
		}
	}
	return m
}

// ghHunks returns the hunks of GitHub's diff of the pull request at
// snapshot s, which is the three-dot diff from the merge base, with three
// lines of context and rename detection, as git produces it.
func (pr *PR) ghHunks(ctx context.Context, s *Snapshot) (map[string][]hunk, error) {
	out, err := pr.Git.run(ctx, "diff", "--no-color", "--no-ext-diff", "-M", "-U3", s.Base, s.Head)
	if err != nil {
		return nil, err
	}
	return parseHunks(string(out)), nil
}

// A ghPlace is where a draft thread lands on GitHub.
type ghPlace struct {
	Path      string
	Line      int
	StartLine int
	Side      string // "LEFT" or "RIGHT"
	File      bool   // a comment on the whole file
	InBody    bool   // no place in GitHub's diff: goes in the review body
	Body      string // the comment, with a quote of its line when moved
}

// placeDraft works out where a draft thread goes when published against
// snapshot latest, whose GitHub diff has the given hunks.
func placeDraft(ctx context.Context, pr *PR, dt *DraftThread, latest *Snapshot, hunks map[string][]hunk) ghPlace {
	p := ghPlace{Path: dt.File, Side: "RIGHT", Body: dt.Body}
	fileHunks, inDiff := hunks[dt.File]
	snap := pr.SnapshotByHead(dt.Head)
	if snap == nil {
		snap = latest
	}

	// Where the comment was written, and where it might be now: the same
	// side of the newest snapshot, or for a comment on one commit of a
	// stack, the newest head first, since the line a commit's parent has
	// was as likely written by an earlier commit as found in the base.
	type place struct {
		rev, side string
	}
	src := snap.Head
	dsts := []place{{latest.Head, "RIGHT"}}
	radius := anchorSearch
	switch {
	case dt.Commit != "":
		src = dt.Commit
		if dt.Side == "old" {
			src = dt.Commit + "^"
			dsts = append(dsts, place{latest.Base, "LEFT"})
		}
		// Line numbers in one commit of a stack can be far from where
		// the line ends up, so look for it anywhere.
		radius = -1
	case dt.Side == "old":
		src = snap.Base
		dsts = []place{{latest.Base, "LEFT"}}
	}
	srcLines := pr.lines(ctx, src, dt.File)
	quote := quoteLines(dt, srcLines, snap, latest)
	if dt.Line > 0 {
		t := &Thread{Line: dt.Line, Anchor: dt.Anchor}
		if t.Anchor == "" && dt.Line <= len(srcLines) {
			t.Anchor = srcLines[dt.Line-1]
		}
		for _, d := range dsts {
			line, start := dt.Line, dt.StartLine
			if d.rev != src {
				n, stale := AnchorWithin(t, pr.lines(ctx, d.rev, dt.File), radius)
				if stale {
					continue
				}
				if start > 0 {
					start += n - line
				}
				line = n
			}
			if inHunk(fileHunks, d.side, start, line) {
				p.Line, p.Side = line, d.side
				if start > 0 && start != line {
					p.StartLine = start
				}
				if dt.Commit != "" && dt.Commit != latest.Head {
					// Say which commit it was about, which the line alone
					// no longer does.
					p.Body = fmt.Sprintf("(On commit %s.)\n\n%s", shortRev(dt.Commit), dt.Body)
				}
				return p
			}
		}
	}
	p.Body = quote + dt.Body
	if inDiff {
		p.File = true
	} else {
		p.InBody = true
	}
	return p
}

// inHunk reports whether lines start..line (start 0 meaning just line)
// lie on the given side of a single hunk.
func inHunk(hs []hunk, side string, start, line int) bool {
	if start <= 0 {
		start = line
	}
	for _, h := range hs {
		lo, hi := h.NewStart, h.NewEnd
		if side == "LEFT" {
			lo, hi = h.OldStart, h.OldEnd
		}
		if lo <= start && line <= hi {
			return true
		}
	}
	return false
}

// quoteLines quotes the lines a comment was written about, for a comment
// that cannot be attached to them on GitHub, so that it still says what
// it is about.
func quoteLines(dt *DraftThread, lines []string, snap, latest *Snapshot) string {
	file, start, line := dt.File, dt.StartLine, dt.Line
	in := ""
	switch {
	case dt.Commit != "":
		in = " in commit " + shortRev(dt.Commit)
		if dt.Side == "old" {
			in = " in the parent of commit " + shortRev(dt.Commit)
		}
	case snap != latest:
		in = fmt.Sprintf(" in snapshot %d (%s)", snap.N, snap.ShortRev())
		if dt.Side == "old" {
			in = fmt.Sprintf(" of the base of snapshot %d", snap.N)
		}
	case dt.Side == "old":
		in = " of the base"
	}
	if line <= 0 {
		if in == "" {
			return ""
		}
		return fmt.Sprintf("(On `%s`%s.)\n\n", file, in)
	}
	if start <= 0 || start > line {
		start = line
	}
	where := fmt.Sprintf("line %d", line)
	if start != line {
		where = fmt.Sprintf("lines %d-%d", start, line)
	}
	var code []string
	for n := start; n <= line && n <= len(lines); n++ {
		code = append(code, lines[n-1])
	}
	fence := "```"
	for strings.Contains(strings.Join(code, "\n"), fence) {
		fence += "`"
	}
	return fmt.Sprintf("`%s` %s%s:\n%s\n%s\n%s\n\n", file, where, in, fence, strings.Join(code, "\n"), fence)
}

// A publishResult says what publishing did.
type publishResult struct {
	Published []string // draft IDs now on GitHub
	ReviewURL string
}

// publish sends the drafts to GitHub as one review with the given event
// ("COMMENT", "APPROVE", or "REQUEST_CHANGES") and body.
func (s *server) publish(ctx context.Context, pr *PR, d *Drafts, event, body string) (*publishResult, error) {
	if s.gh.Token == "" {
		return nil, errNoToken
	}
	latest := pr.SnapshotByHead(pr.Pull.Head.SHA)
	if latest == nil || latest.Missing {
		return nil, fmt.Errorf("the pull request's head commit %s is not available", shortRev(pr.Pull.Head.SHA))
	}
	if event == "" {
		event = "COMMENT"
	}
	body = strings.TrimSpace(body)
	if event == "COMMENT" && body == "" && d.Count() == 0 {
		return nil, fmt.Errorf("nothing to publish")
	}
	if event == "APPROVE" && pr.Extra != nil && pr.Extra.Viewer == pr.Pull.User.Login {
		return nil, fmt.Errorf("GitHub does not allow approving your own pull request")
	}
	hunks, err := pr.ghHunks(ctx, latest)
	if err != nil {
		return nil, err
	}

	// Work out where everything goes before touching GitHub at all.
	type threadReq struct {
		dt    *DraftThread
		place ghPlace
	}
	var threads []threadReq
	var bodyParts []string
	if body != "" {
		bodyParts = append(bodyParts, body)
	}
	res := &publishResult{}
	for _, dt := range d.Threads {
		p := placeDraft(ctx, pr, dt, latest, hunks)
		if p.InBody {
			bodyParts = append(bodyParts, p.Body)
			res.Published = append(res.Published, dt.ID)
			continue
		}
		threads = append(threads, threadReq{dt, p})
	}
	type replyReq struct {
		r *DraftReply
		t *Thread
	}
	var replies []replyReq
	for _, r := range d.Replies {
		t := pr.Thread(r.ThreadID)
		if t == nil || t.NodeID == "" {
			return nil, fmt.Errorf("cannot reply to thread %s: it is gone from GitHub, or its ID is unknown", r.ThreadID)
		}
		replies = append(replies, replyReq{r, t})
	}

	// A pending review the viewer started on github.com must be used, as
	// GitHub allows only one; it gets this review's comments added to it.
	reviewID := ""
	created := false
	if pr.Extra != nil {
		reviewID = pr.Extra.PendingReview
	}
	if reviewID == "" {
		var r struct {
			AddPullRequestReview struct {
				PullRequestReview struct{ ID string }
			}
		}
		err := s.gh.Query(ctx, `mutation($in: AddPullRequestReviewInput!) {
			addPullRequestReview(input: $in) { pullRequestReview { id } } }`,
			map[string]any{"in": map[string]any{"pullRequestId": pr.Pull.NodeID, "commitOID": latest.Head}}, &r)
		if err != nil {
			return nil, fmt.Errorf("starting review: %w", err)
		}
		reviewID = r.AddPullRequestReview.PullRequestReview.ID
		created = true
	}
	abandon := func(err error) error {
		if created {
			s.gh.Query(ctx, `mutation($in: DeletePullRequestReviewInput!) {
				deletePullRequestReview(input: $in) { clientMutationId } }`,
				map[string]any{"in": map[string]any{"pullRequestReviewId": reviewID}}, nil)
		}
		return err
	}

	// Threads and replies, all in one request.
	newThreadIDs := map[string]string{}
	if len(threads)+len(replies) > 0 {
		var decls, fields []string
		vars := map[string]any{}
		for i, t := range threads {
			in := map[string]any{
				"pullRequestReviewId": reviewID,
				"path":                t.place.Path,
				"body":                t.place.Body,
			}
			if t.place.File {
				in["subjectType"] = "FILE"
			} else {
				in["line"] = t.place.Line
				in["side"] = t.place.Side
				if t.place.StartLine > 0 {
					in["startLine"] = t.place.StartLine
					in["startSide"] = t.place.Side
				}
			}
			n := fmt.Sprintf("t%d", i)
			vars[n] = in
			decls = append(decls, "$"+n+": AddPullRequestReviewThreadInput!")
			fields = append(fields, n+": addPullRequestReviewThread(input: $"+n+") { thread { id } }")
		}
		for i, r := range replies {
			n := fmt.Sprintf("r%d", i)
			vars[n] = map[string]any{
				"pullRequestReviewId":       reviewID,
				"pullRequestReviewThreadId": r.t.NodeID,
				"body":                      r.r.Body,
			}
			decls = append(decls, "$"+n+": AddPullRequestReviewThreadReplyInput!")
			fields = append(fields, n+": addPullRequestReviewThreadReply(input: $"+n+") { comment { id } }")
		}
		q := "mutation(" + strings.Join(decls, ", ") + ") {\n" + strings.Join(fields, "\n") + "\n}"
		var out map[string]struct {
			Thread *struct{ ID string }
		}
		if err := s.gh.Query(ctx, q, vars, &out); err != nil {
			return nil, abandon(fmt.Errorf("adding comments: %w", err))
		}
		for i, t := range threads {
			if o := out[fmt.Sprintf("t%d", i)]; o.Thread != nil {
				newThreadIDs[t.dt.ID] = o.Thread.ID
			}
		}
	}

	// Submit, and resolve whatever the drafts said to, in one request.
	vars := map[string]any{"s": map[string]any{
		"pullRequestReviewId": reviewID,
		"event":               event,
		"body":                strings.Join(bodyParts, "\n\n---\n\n"),
	}}
	decls := []string{"$s: SubmitPullRequestReviewInput!"}
	fields := []string{"s: submitPullRequestReview(input: $s) { pullRequestReview { url } }"}
	resolve := func(threadID string) {
		n := fmt.Sprintf("x%d", len(decls))
		vars[n] = map[string]any{"threadId": threadID}
		decls = append(decls, "$"+n+": ResolveReviewThreadInput!")
		fields = append(fields, n+": resolveReviewThread(input: $"+n+") { thread { id } }")
	}
	for _, t := range threads {
		if id := newThreadIDs[t.dt.ID]; id != "" && t.dt.Resolved {
			resolve(id)
		}
	}
	for _, r := range replies {
		if r.r.Resolve && !r.t.Resolved {
			resolve(r.t.NodeID)
		}
	}
	var out struct {
		S struct {
			PullRequestReview struct{ URL string }
		}
	}
	q := "mutation(" + strings.Join(decls, ", ") + ") {\n" + strings.Join(fields, "\n") + "\n}"
	if err := s.gh.Query(ctx, q, vars, &out); err != nil {
		if out.S.PullRequestReview.URL == "" {
			return nil, abandon(fmt.Errorf("submitting review: %w", err))
		}
		// Submitted, but a resolve failed. The comments are out there, so
		// the drafts must go regardless.
	}
	res.ReviewURL = out.S.PullRequestReview.URL
	for _, t := range threads {
		res.Published = append(res.Published, t.dt.ID)
	}
	for _, r := range replies {
		res.Published = append(res.Published, r.r.ID)
	}
	return res, nil
}

// setResolved resolves or unresolves a published thread right away, as
// GitHub's own button does.
func (s *server) setResolved(ctx context.Context, nodeID string, on bool) error {
	name := "resolveReviewThread"
	typ := "ResolveReviewThreadInput"
	if !on {
		name, typ = "unresolveReviewThread", "UnresolveReviewThreadInput"
	}
	q := fmt.Sprintf("mutation($in: %s!) { %s(input: $in) { thread { id isResolved } } }", typ, name)
	return s.gh.Query(ctx, q, map[string]any{"in": map[string]any{"threadId": nodeID}}, nil)
}
