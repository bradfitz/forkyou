// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The commit message is reviewed as a file, as in Gerrit, under the
// name /COMMIT_MSG: listed first among a commit's files, and among a
// snapshot's, where it holds the messages of all the snapshot's commits
// in order. Against the parent it reads as a wholly new file; against an
// earlier snapshot it diffs normally, which is how a reworded message
// shows up.
//
// GitHub has nowhere to put a comment on a commit message, so such
// comments are published in the body of the review, each in a section a
// person can read on github.com and this program can read back:
//
//	<!-- forkyou:thread id=3f9a1c2e commit=<sha> file=%2FCOMMIT_MSG line=3 -->
//	`/COMMIT_MSG` line 3 of commit 3f9a1c2 ("guestnet: add DenyLocal"):
//	```
//	the line commented on
//	```
//
//	The comment.
//	<!-- forkyou:end -->
//
// The markers are HTML comments, which GitHub keeps in the body and does
// not show. A reply is a section starting with a reply marker naming the
// thread, which also says whether it leaves the thread resolved. Comments
// that cannot go on a line of GitHub's diff for any other reason, such as
// one on a file the pull request does not touch, are published the same
// way, so that they come back as threads on their lines too.

const commitMsgPath = "/COMMIT_MSG"

// isCommitMsg reports whether path names the commit message.
func isCommitMsg(path string) bool { return path == commitMsgPath }

// A msgSeg is one commit's message within a snapshot's /COMMIT_MSG.
type msgSeg struct {
	SHA      string
	Identity string
	Start    int // 0-based line at which the message starts
	Lines    int
}

// snapshotMsg returns the /COMMIT_MSG of a snapshot: the messages of its
// commits, oldest first, each but a lone one headed by a line saying
// which commit it is. The headers name commits by position rather than
// ID, which a rebase would change on every line of them.
func (pr *PR) snapshotMsg(ctx context.Context, s *Snapshot) (text string, segs []msgSeg) {
	cs := pr.Commits(ctx, s)
	var b strings.Builder
	line := 0
	for i, c := range cs {
		if len(cs) > 1 {
			if i > 0 {
				b.WriteString("\n")
				line++
			}
			fmt.Fprintf(&b, "=== commit %d of %d ===\n", i+1, len(cs))
			line++
		}
		msg := commitMsgText(c.Commit)
		n := strings.Count(msg, "\n")
		segs = append(segs, msgSeg{SHA: c.SHA, Identity: c.Identity(), Start: line, Lines: n})
		b.WriteString(msg)
		line += n
	}
	return b.String(), segs
}

// commitMsgText is a commit's /COMMIT_MSG.
func commitMsgText(c *Commit) string {
	return strings.TrimRight(c.Message, "\n") + "\n"
}

// segAt returns the segment holding 1-based line n, or nil.
func segAt(segs []msgSeg, n int) *msgSeg {
	for i := range segs {
		if s := &segs[i]; n > s.Start && n <= s.Start+s.Lines {
			return s
		}
	}
	return nil
}

// segFor returns the segment for a thread's commit: the commit itself,
// or failing that a version of it, matched by identity. It reports
// whether the match was the commit itself.
func segFor(segs []msgSeg, sha, identity string) (*msgSeg, bool) {
	for i := range segs {
		if segs[i].SHA == sha {
			return &segs[i], true
		}
	}
	for i := range segs {
		if identity != "" && segs[i].Identity == identity {
			return &segs[i], false
		}
	}
	return nil, false
}

// msgContents returns the two sides of /COMMIT_MSG in a view.
func (v *View) msgContents(ctx context.Context) (old, new []byte) {
	if v.Commit != nil {
		return nil, []byte(commitMsgText(v.Commit.Commit))
	}
	n, _ := v.PR.snapshotMsg(ctx, v.Target)
	if v.Base != nil {
		o, _ := v.PR.snapshotMsg(ctx, v.Base)
		return []byte(o), []byte(n)
	}
	return nil, []byte(n)
}

// msgFile returns the /COMMIT_MSG entry for a view's file list.
func (v *View) msgFile(ctx context.Context) *ViewFile {
	old, new := v.msgContents(ctx)
	f := &ViewFile{Path: commitMsgPath, Status: 'A', Own: true}
	if old != nil {
		f.Status = 'M'
		if string(old) == string(new) {
			f.Status = 0
		}
	}
	for _, r := range Diff(old, new).Rows {
		if r.Kind == RowEqual {
			continue
		}
		if r.L.Num > 0 {
			f.Deleted++
		}
		if r.R.Num > 0 {
			f.Added++
		}
	}
	return f
}

// commitMessage returns the message of any commit, cached.
func (pr *PR) commitMessage(ctx context.Context, sha string) string {
	pr.mu.Lock()
	m, ok := pr.messages[sha]
	pr.mu.Unlock()
	if ok {
		return m
	}
	out, err := pr.Git.run(ctx, "log", "-1", "--format=%B", sha)
	if err != nil {
		log.Printf("%v: %v", pr.Key, err)
		return ""
	}
	m = strings.TrimRight(string(out), "\n")
	pr.mu.Lock()
	if pr.messages == nil {
		pr.messages = map[string]string{}
	}
	pr.messages[sha] = m
	pr.mu.Unlock()
	return m
}

// identityOf returns the identity of any commit (see CommitInfo.Identity).
func (pr *PR) identityOf(ctx context.Context, sha string) string {
	msg := pr.commitMessage(ctx, sha)
	subj, _, _ := strings.Cut(msg, "\n")
	return (&CommitInfo{Commit: &Commit{Message: msg, Subject: subj}}).Identity()
}

// ---- publishing to and reading back from review bodies --------------------

// A bodyMarker is the parsed form of a forkyou:thread or forkyou:reply
// marker.
type bodyMarker struct {
	Kind      string // "thread" or "reply"
	ID        string // the thread's ID (for a reply, the thread replied to)
	Head      string // snapshot head written against
	Commit    string // the commit, for a comment on one commit
	File      string
	Side      string // "old" or "new"
	Line      int
	StartLine int
	Resolved  bool
}

func (m *bodyMarker) String() string {
	v := url.Values{}
	v.Set("id", m.ID)
	if m.Kind == "thread" {
		v.Set("file", m.File)
		if m.Head != "" {
			v.Set("head", m.Head)
		}
		if m.Commit != "" {
			v.Set("commit", m.Commit)
		}
		if m.Side == "old" {
			v.Set("side", "old")
		}
		if m.Line > 0 {
			v.Set("line", strconv.Itoa(m.Line))
		}
		if m.StartLine > 0 {
			v.Set("start", strconv.Itoa(m.StartLine))
		}
	}
	if m.Resolved {
		v.Set("resolved", "1")
	} else if m.Kind == "reply" {
		v.Set("resolved", "0")
	}
	// Spaces, not ampersands, between pairs, which reads better in the raw
	// body. Every value is escaped, so none can contain one, nor "-->".
	var parts []string
	for _, k := range []string{"id", "head", "commit", "file", "side", "line", "start", "resolved"} {
		if x := v.Get(k); x != "" {
			parts = append(parts, k+"="+url.QueryEscape(x))
		}
	}
	return "<!-- forkyou:" + m.Kind + " " + strings.Join(parts, " ") + " -->"
}

const bodyEnd = "<!-- forkyou:end -->"

var markerRE = regexp.MustCompile(`<!-- forkyou:(thread|reply) ([^>]*?) -->`)

// A bodySection is one comment found in a review body.
type bodySection struct {
	bodyMarker
	Body string // the comment, without the quote heading it
}

// parseBody finds the forkyou sections in a review body.
func parseBody(body string) []bodySection {
	var out []bodySection
	body = strings.ReplaceAll(body, "\r\n", "\n")
	for _, loc := range markerRE.FindAllStringSubmatchIndex(body, -1) {
		m := bodyMarker{Kind: body[loc[2]:loc[3]]}
		for _, kv := range strings.Fields(body[loc[4]:loc[5]]) {
			k, raw, _ := strings.Cut(kv, "=")
			x, err := url.QueryUnescape(raw)
			if err != nil {
				continue
			}
			switch k {
			case "id":
				m.ID = x
			case "head":
				m.Head = x
			case "commit":
				m.Commit = x
			case "file":
				m.File = x
			case "side":
				m.Side = x
			case "line":
				m.Line, _ = strconv.Atoi(x)
			case "start":
				m.StartLine, _ = strconv.Atoi(x)
			case "resolved":
				m.Resolved = x == "1"
			}
		}
		if m.ID == "" || m.Kind == "thread" && m.File == "" {
			continue
		}
		if m.Side == "" {
			m.Side = "new"
		}
		rest := body[loc[1]:]
		if i := strings.Index(rest, bodyEnd); i >= 0 {
			rest = rest[:i]
		} else if l := markerRE.FindStringIndex(rest); l != nil {
			rest = rest[:l[0]]
		}
		out = append(out, bodySection{m, stripQuote(strings.TrimSpace(rest))})
	}
	return out
}

// stripQuote removes the heading a section starts with, which says where
// the comment is for people reading it on GitHub and is shown here by
// placing the comment on its line instead. A heading is either one line
// in parentheses, or a line ending in a colon followed by a fenced quote
// of the lines commented on, as quoteLines and commitMsgQuote write them.
func stripQuote(s string) string {
	head, rest, ok := strings.Cut(s, "\n")
	if !ok {
		return s
	}
	if strings.HasPrefix(head, "(") && strings.HasSuffix(head, ")") {
		return strings.TrimSpace(rest)
	}
	if !strings.HasSuffix(head, ":") || !strings.HasPrefix(rest, "```") {
		return s
	}
	n := strings.IndexFunc(rest, func(r rune) bool { return r != '`' })
	if n < 0 {
		return s
	}
	fence := rest[:n]
	end := strings.Index(rest[n:], "\n"+fence+"\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n"+fence) {
			return ""
		}
		return s
	}
	return strings.TrimSpace(rest[n+end+1+len(fence):])
}

// newThreadID returns an ID for a thread published in a review body.
func newThreadID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// bodyThreadSection formats a draft thread as a review body section, with
// text being its heading and body.
func bodyThreadSection(dt *DraftThread, text string) string {
	m := &bodyMarker{Kind: "thread", ID: newThreadID(), Head: dt.Head, Commit: dt.Commit,
		File: dt.File, Side: dt.Side, Line: dt.Line, StartLine: dt.StartLine, Resolved: dt.Resolved}
	return m.String() + "\n" + text + "\n" + bodyEnd
}

// bodyReplySection formats a draft reply to a body thread.
func bodyReplySection(t *Thread, r *DraftReply) string {
	m := &bodyMarker{Kind: "reply", ID: strings.TrimPrefix(t.ID, "b"), Resolved: r.Resolve}
	quote := ""
	if len(t.Comments) > 0 {
		first := t.Comments[0]
		snippet, _, _ := strings.Cut(first.Body, "\n")
		if len(snippet) > 80 {
			snippet = snippet[:80] + "…"
		}
		quote = fmt.Sprintf("(Replying to %s on `%s`: %q.)\n\n", first.Author, t.Where(), snippet)
	}
	return m.String() + "\n" + quote + r.Body + "\n" + bodyEnd
}

// commitMsgQuote is the heading of a published comment on a commit message.
func commitMsgQuote(ctx context.Context, pr *PR, dt *DraftThread) string {
	msg := pr.commitMessage(ctx, dt.Commit)
	lines := strings.Split(msg, "\n")
	subj := lines[0]
	start, line := dt.StartLine, dt.Line
	if line <= 0 {
		return fmt.Sprintf("On the commit message of %s (%q):\n\n", shortRev(dt.Commit), subj)
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
	return fmt.Sprintf("Commit message of %s (%q), %s:\n%s\n%s\n%s\n\n",
		shortRev(dt.Commit), subj, where, fence, strings.Join(code, "\n"), fence)
}

// buildBodyThreads reads back the threads published in review bodies.
func buildBodyThreads(ctx context.Context, pr *PR) []*Thread {
	byID := map[string]*Thread{}
	var threads []*Thread
	reviews := slices.Clone(pr.Reviews)
	slices.SortStableFunc(reviews, func(a, b GHReview) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	for _, r := range reviews {
		if r.State == "PENDING" {
			continue
		}
		for _, sec := range parseBody(r.Body) {
			c := &Comment{
				ID:      fmt.Sprintf("r%d-%s", r.ID, sec.ID),
				Author:  r.User.Login,
				Avatar:  r.User.AvatarURL,
				Body:    sec.Body,
				Created: r.SubmittedAt,
				URL:     r.HTMLURL,
			}
			if sec.Kind == "reply" {
				if t := byID[sec.ID]; t != nil {
					t.Comments = append(t.Comments, c)
					t.Resolved = sec.Resolved
				}
				continue
			}
			if byID[sec.ID] != nil {
				continue
			}
			t := &Thread{
				ID:        "b" + sec.ID,
				Head:      sec.Head,
				Commit:    sec.Commit,
				File:      sec.File,
				Side:      sec.Side,
				Line:      sec.Line,
				StartLine: sec.StartLine,
				Resolved:  sec.Resolved,
				URL:       r.HTMLURL,
				Comments:  []*Comment{c},
				InBody:    true,
			}
			if s := pr.SnapshotByHead(t.Head); s != nil {
				t.SnapshotN = s.N
			} else if s := pr.SnapshotByHead(r.CommitID); s != nil {
				t.Head, t.SnapshotN = s.Head, s.N
			}
			t.Anchor = pr.anchorText(ctx, t)
			byID[sec.ID] = t
			threads = append(threads, t)
		}
	}
	return threads
}

// anchorText reads the text of a thread's line from the commit it was
// written on.
func (pr *PR) anchorText(ctx context.Context, t *Thread) string {
	if t.Line <= 0 {
		return ""
	}
	var lines []string
	switch {
	case isCommitMsg(t.File) && t.Commit != "":
		lines = strings.Split(pr.commitMessage(ctx, t.Commit), "\n")
	case t.Commit != "":
		rev := t.Commit
		if t.Side == "old" {
			rev += "^"
		}
		lines = pr.lines(ctx, rev, t.File)
	case t.Head != "":
		rev := t.Head
		if t.Side == "old" {
			if s := pr.SnapshotByHead(t.Head); s != nil {
				rev = s.Base
			}
		}
		lines = pr.lines(ctx, rev, t.File)
	}
	if t.Line <= len(lines) {
		return lines[t.Line-1]
	}
	return ""
}

// placeMsgThreads places the threads on /COMMIT_MSG in a view. Threads
// are kept by commit, so their lines are translated to where that
// commit's message sits in the view's /COMMIT_MSG; a thread written on
// an earlier version of a commit follows it by the commit's identity.
func (v *View) placeMsgThreads(ctx context.Context, all []*Thread, oldLines, newLines []string) (left, right []*Thread) {
	var newSegs, oldSegs []msgSeg
	if v.Commit != nil {
		newSegs = []msgSeg{{SHA: v.Commit.SHA, Identity: v.Commit.Identity(), Lines: len(newLines)}}
	} else {
		_, newSegs = v.PR.snapshotMsg(ctx, v.Target)
		if v.Base != nil {
			_, oldSegs = v.PR.snapshotMsg(ctx, v.Base)
		}
	}
	for _, t := range all {
		if !isCommitMsg(t.File) {
			continue
		}
		t = t.clone()
		t.Other, t.Stale = false, false
		id := ""
		if t.Commit != "" {
			id = v.PR.identityOf(ctx, t.Commit)
		}
		if seg, exact := segFor(newSegs, t.Commit, id); seg != nil {
			if os, oexact := segFor(oldSegs, t.Commit, id); os != nil && oexact && !exact {
				// Written on the version on the left.
				t.Line, t.StartLine = shift(t, os.Start)
				t.ShowLine, t.Stale = AnchorWithin(t, oldLines, -1)
				if !t.Stale {
					left = append(left, t)
				}
				continue
			}
			t.Other = !exact
			t.Line, t.StartLine = shift(t, seg.Start)
			t.ShowLine, t.Stale = AnchorWithin(t, newLines, -1)
			if t.Line == 0 && seg.Start > 0 {
				t.ShowLine = seg.Start + 1 // a comment on a whole message goes on its first line
			}
			right = append(right, t)
		}
	}
	return left, right
}

// shift returns a thread's lines moved down by n.
func shift(t *Thread, n int) (line, start int) {
	line, start = t.Line, t.StartLine
	if line > 0 {
		line += n
	}
	if start > 0 {
		start += n
	}
	return line, start
}

// msgTarget works out which commit a comment on line n of the view's
// /COMMIT_MSG is about, and its line within that commit's message.
func (p *prPage) msgTarget(ctx context.Context, side string, line int) (commit string, msgLine int, err error) {
	if p.Commit != nil {
		if side == "left" {
			return "", 0, badRequest("a commit message has no left side against the parent")
		}
		return p.Commit.SHA, line, nil
	}
	s := p.Target
	if side == "left" {
		if p.Base == nil {
			return "", 0, badRequest("a commit message has no left side against the parent")
		}
		s = p.Base
	}
	_, segs := p.PR.snapshotMsg(ctx, s)
	if line == 0 {
		if len(segs) == 1 {
			return segs[0].SHA, 0, nil
		}
		return "", 0, badRequest("comment on one commit's message, not all of them")
	}
	seg := segAt(segs, line)
	if seg == nil {
		return "", 0, badRequest("line %d of /COMMIT_MSG is not part of any commit's message", line)
	}
	return seg.SHA, line - seg.Start, nil
}
