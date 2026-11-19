// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
)

// A View is a comparison between two versions of a pull request: a
// snapshot against its own parent, which is the whole change as of that
// push, or one snapshot against another, which is what changed between
// two pushes.
//
// Comparing two snapshots after a rebase is where GitHub's own interface
// gives up, showing the base branch's every change as though the pull
// request had made it. Here, as in Gerrit, a line that came along with
// the rebase is drawn in muted colors, a file touched only by the rebase
// is left out of the list, and a diff count includes only lines the pull
// request itself wrote.
type View struct {
	PR       *PR
	Base     *Snapshot // nil when comparing against Target's parent
	Target   *Snapshot
	AutoBase bool // Base was chosen as the last snapshot marked reviewed
	Files    []*ViewFile
	// Commit, if set, narrows the view to one commit of Target,
	// compared with its parent.
	Commit   *CommitInfo
	NCommits int // commits in Target, when Commit is set
}

// A ViewFile is one file in a view.
type ViewFile struct {
	Path   string
	Status byte // 'A', 'D', 'M', 'T', or 0 for a file listed only for its comments
	OldOID string
	NewOID string

	// Own reports whether the pull request itself touches the file in
	// either snapshot being compared. A file it touches in neither is in
	// the list only because a rebase brought changes to it.
	Own bool
	// RebaseOnly reports that every changed line in the file came along
	// with a rebase.
	RebaseOnly bool
	Added      int
	Deleted    int
	Binary     bool
}

// Char returns the one-letter status shown in the file list.
func (f *ViewFile) Char() string {
	if f.Status == 0 {
		return ""
	}
	return string(f.Status)
}

// LeftRev is the commit shown on the left.
func (v *View) LeftRev() string {
	if v.Commit != nil {
		return v.Commit.Parents[0]
	}
	if v.Base != nil {
		return v.Base.Head
	}
	return v.Target.Base
}

// RightRev is the commit shown on the right.
func (v *View) RightRev() string {
	if v.Commit != nil {
		return v.Commit.SHA
	}
	return v.Target.Head
}

// Rebased reports whether the two sides sit on different parents, which
// is when lines can have come along with a rebase.
func (v *View) Rebased() bool {
	return v.Commit == nil && v.Base != nil && v.Base.Base != v.Target.Base
}

// BaseN returns the number of the base snapshot, or 0 for the parent.
func (v *View) BaseN() int {
	if v.Base == nil {
		return 0
	}
	return v.Base.N
}

// TargetN returns the number of the target snapshot.
func (v *View) TargetN() int { return v.Target.N }

// LatestN returns the number of the newest snapshot.
func (v *View) LatestN() int { return v.PR.Latest().N }

// File returns the file with the given path, or nil.
func (v *View) File(path string) *ViewFile {
	for _, f := range v.Files {
		if f.Path == path {
			return f
		}
	}
	return nil
}

// errNoSnapshot is returned for a pull request with nothing fetchable.
var errNoSnapshot = fmt.Errorf("no snapshot of this pull request could be fetched")

// newView builds the comparison a page asked for. base is "" to choose
// automatically, "parent", or a snapshot number; target is "" for the
// newest snapshot, or a number. threads lists every thread, so that a
// file carrying a comment stays in the list even when its diff is empty.
func newView(ctx context.Context, pr *PR, base, target, commit string, marks *Reviewed, threads []*Thread) (*View, error) {
	v := &View{PR: pr}
	if n, err := strconv.Atoi(target); err == nil {
		v.Target = pr.Snapshot(n)
	}
	if v.Target == nil || v.Target.Missing {
		v.Target = nil
		for i := len(pr.Snapshots) - 1; i >= 0; i-- {
			if !pr.Snapshots[i].Missing {
				v.Target = pr.Snapshots[i]
				break
			}
		}
	}
	if v.Target == nil {
		return nil, errNoSnapshot
	}
	switch base {
	case "parent":
	case "":
		// Gerrit's habit: show what has changed since the reviewer last
		// looked, which is the newest earlier snapshot they marked reviewed.
		for i := v.Target.N - 1; i >= 1; i-- {
			s := pr.Snapshot(i)
			if !s.Missing && marks != nil && marks.Snapshots[s.Head] {
				v.Base, v.AutoBase = s, true
				break
			}
		}
	default:
		if n, err := strconv.Atoi(base); err == nil && n < v.Target.N {
			if s := pr.Snapshot(n); s != nil && !s.Missing {
				v.Base = s
			}
		}
	}
	if commit != "" {
		v.Commit = pr.CommitBySHA(ctx, v.Target, commit)
		if v.Commit == nil || v.Commit.Merge {
			return nil, notFound("no commit %s in snapshot %d", commit, v.Target.N)
		}
		v.Base, v.AutoBase = nil, false
		v.NCommits = len(pr.Commits(ctx, v.Target))
		for _, f := range v.Commit.Files {
			if isCommitMsg(f.Path) {
				v.Files = append(v.Files, &ViewFile{Path: f.Path, Status: f.Status, Own: true, Added: f.Added})
				continue
			}
			v.Files = append(v.Files, &ViewFile{Path: f.Path, Status: f.Status, OldOID: f.OldOID, NewOID: f.NewOID, Own: true,
				Added: f.Added, Deleted: f.Deleted, Binary: f.Binary})
		}
		return v, nil
	}
	if err := v.listFiles(ctx, threads); err != nil {
		return nil, err
	}
	v.Files = append([]*ViewFile{v.msgFile(ctx)}, v.Files...)
	return v, nil
}

// listFiles fills in v.Files.
func (v *View) listFiles(ctx context.Context, threads []*Thread) error {
	git := v.PR.Git
	changes, err := git.DiffTrees(ctx, v.LeftRev(), v.RightRev())
	if err != nil {
		return err
	}
	own := map[string]bool{}
	if v.Rebased() {
		for _, s := range []*Snapshot{v.Base, v.Target} {
			fs, err := git.DiffTrees(ctx, s.Base, s.Head)
			if err != nil {
				return err
			}
			for _, f := range fs {
				own[f.Path] = true
			}
		}
	}
	var oids, rebasedPaths []string
	for _, c := range changes {
		if c.Submodule {
			continue
		}
		f := &ViewFile{Path: c.Path, Status: c.Status, OldOID: c.OldOID, NewOID: c.NewOID, Own: true}
		if v.Rebased() {
			f.Own = own[c.Path]
			f.RebaseOnly = !f.Own
			if f.Own {
				rebasedPaths = append(rebasedPaths, c.Path)
			}
		}
		v.Files = append(v.Files, f)
		if f.Own {
			oids = append(oids, c.OldOID, c.NewOID)
		}
	}
	// One round trip for everything the counts below will read.
	if err := git.Prefetch(ctx, []string{v.LeftRev(), v.RightRev()}, oids); err != nil {
		return err
	}
	if len(rebasedPaths) > 0 {
		v.PR.prefetchPaths(ctx, []string{v.Base.Base, v.Target.Base}, rebasedPaths)
	}
	for _, f := range v.Files {
		if !f.Own {
			continue
		}
		fd := v.Diff(ctx, f)
		f.Binary = fd.Binary
		for _, r := range fd.Rows {
			if r.L.Num > 0 && r.Kind != RowEqual && !r.RebasedL {
				f.Deleted++
			}
			if r.R.Num > 0 && r.Kind != RowEqual && !r.RebasedR {
				f.Added++
			}
		}
		if v.Rebased() {
			f.RebaseOnly = AllRebased(fd.Rows)
		}
	}

	// A comment keeps its file in the list however the lines got there,
	// and even when the file has no diff at all in this comparison.
	listed := map[string]bool{}
	for _, f := range v.Files {
		listed[f.Path] = true
	}
	var extra []*ViewFile
	for _, t := range threads {
		if !listed[t.File] && !isCommitMsg(t.File) && v.PR.blob(ctx, v.RightRev(), t.File) != nil {
			listed[t.File] = true
			extra = append(extra, &ViewFile{Path: t.File})
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Path < extra[j].Path })
	v.Files = append(v.Files, extra...)
	return nil
}

// Contents returns the two sides of a file.
func (v *View) Contents(ctx context.Context, f *ViewFile) (old, new []byte) {
	if isCommitMsg(f.Path) {
		return v.msgContents(ctx)
	}
	if f.Status == 0 {
		data := v.PR.blob(ctx, v.RightRev(), f.Path)
		return v.PR.blob(ctx, v.LeftRev(), f.Path), data
	}
	return v.PR.blob(ctx, f.OldOID, ""), v.PR.blob(ctx, f.NewOID, "")
}

// Diff returns the diff of a file, with lines that came along with a
// rebase marked. Diffs are cached, since a page draws the file list's
// counts and then, often, the very same file.
func (v *View) Diff(ctx context.Context, f *ViewFile) *FileDiff {
	if isCommitMsg(f.Path) {
		old, new := v.msgContents(ctx)
		return Diff(old, new)
	}
	if f.Status != 0 && !v.Rebased() {
		return v.PR.blobDiff(ctx, f.OldOID, f.NewOID)
	}
	key := v.LeftRev() + " " + v.RightRev() + " " + f.Path
	v.PR.mu.Lock()
	fd := v.PR.diffs[key]
	v.PR.mu.Unlock()
	if fd != nil {
		return fd
	}
	old, new := v.Contents(ctx, f)
	fd = Diff(old, new)
	if v.Rebased() && f.Status != 0 {
		MarkRebased(fd.Rows, old, new,
			v.PR.blob(ctx, v.Base.Base, f.Path),
			v.PR.blob(ctx, v.Target.Base, f.Path))
	}
	v.PR.mu.Lock()
	if v.PR.diffs == nil {
		v.PR.diffs = map[string]*FileDiff{}
	}
	v.PR.diffs[key] = fd
	v.PR.mu.Unlock()
	return fd
}

// ---- placing threads ------------------------------------------------------------

// anchorSearch is how far from its original line to look for a thread's
// anchor text before calling the thread stale.
const anchorSearch = 25

// Anchor finds the line where a thread should be drawn in a file whose
// contents may have moved on since the comment was written. It looks for
// the exact text the comment was attached to, nearest its original line,
// and reports stale if that text can no longer be found.
func Anchor(t *Thread, lines []string) (line int, stale bool) {
	return AnchorWithin(t, lines, anchorSearch)
}

// AnchorWithin is Anchor looking at most radius lines away, or anywhere
// in the file if radius is negative.
func AnchorWithin(t *Thread, lines []string, radius int) (line int, stale bool) {
	if radius < 0 {
		radius = len(lines) + t.Line
	}
	if t.Line <= 0 {
		return 0, false // a comment on the file as a whole is always in place
	}
	if t.Anchor == "" {
		if t.Line <= len(lines) {
			return t.Line, false
		}
		return 0, true
	}
	if t.Line <= len(lines) && lines[t.Line-1] == t.Anchor {
		return t.Line, false
	}
	for d := 1; d <= radius; d++ {
		for _, n := range [2]int{t.Line - d, t.Line + d} {
			if n >= 1 && n <= len(lines) && lines[n-1] == t.Anchor {
				return n, false
			}
		}
	}
	return 0, true
}

// PlaceThreads picks out the threads on file and says which side of the
// diff each is drawn on and at what line. The threads are copies, so
// placing them disturbs nothing shared.
func (v *View) PlaceThreads(all []*Thread, file string, oldLines, newLines []string) (left, right []*Thread) {
	for _, t := range all {
		if t.File != file {
			continue
		}
		t = t.clone()
		t.Other, t.Stale = false, false
		if v.Commit != nil {
			// A commit's own threads go where they were written. Any
			// other is shown only if its line is here too, as a remark
			// about some other version of the same code: listing every
			// thread that does not fit as stale would bury the commit's
			// own under the rest of the pull request's.
			lines, side := newLines, "new"
			if t.Commit == v.Commit.SHA {
				if t.Side == "old" {
					lines, side = oldLines, "old"
				}
			} else {
				t.Other = true
			}
			t.ShowLine, t.Stale = Anchor(t, lines)
			if t.Stale || (t.Other && t.Line == 0) {
				continue
			}
			if side == "old" {
				left = append(left, t)
			} else {
				right = append(right, t)
			}
			continue
		}
		if t.Commit != "" {
			// Written on one commit; on a snapshot it is a remark about
			// the code as it is now, if that line is still there.
			t.Other = true
			t.ShowLine, t.Stale = Anchor(t, newLines)
			right = append(right, t)
			continue
		}
		switch {
		case t.Head == v.Target.Head && t.Side == "new":
			t.ShowLine, t.Stale = Anchor(t, newLines)
			right = append(right, t)
		case v.Base != nil && t.Head == v.Base.Head && t.Side == "new":
			t.ShowLine, t.Stale = Anchor(t, oldLines)
			left = append(left, t)
		case v.Base == nil && t.Head == v.Target.Head && t.Side == "old":
			t.ShowLine, t.Stale = Anchor(t, oldLines)
			left = append(left, t)
		case v.Base == nil && t.Side == "old":
			// A remark about the parent of some other snapshot, which is
			// most likely still what is on the left.
			t.Other = true
			t.ShowLine, t.Stale = Anchor(t, oldLines)
			left = append(left, t)
		default:
			// A thread from some other snapshot. Show it against the new
			// side, where the reviewer is looking.
			t.Other = true
			t.ShowLine, t.Stale = Anchor(t, newLines)
			right = append(right, t)
		}
	}
	return left, right
}

// ThreadsByLine groups threads by the line they are drawn at. Stale
// threads are grouped at line 0, along with comments on the whole file,
// so that the caller can list them at the top rather than lose them.
func ThreadsByLine(threads []*Thread) map[int][]*Thread {
	m := map[int][]*Thread{}
	for _, t := range threads {
		line := t.ShowLine
		if t.Stale {
			line = 0
		}
		m[line] = append(m[line], t)
	}
	for _, ts := range m {
		slices.SortStableFunc(ts, func(a, b *Thread) int { return a.Comments[0].Created.Compare(b.Comments[0].Created) })
	}
	return m
}

// allThreads returns the published threads with the drafts merged in:
// a draft thread becomes a thread of its own, and a draft reply is added
// to the end of the thread it answers.
func allThreads(pr *PR, d *Drafts) []*Thread {
	var out []*Thread
	replies := map[string]*DraftReply{}
	for _, r := range d.Replies {
		replies[r.ThreadID] = r
	}
	for _, t := range pr.Threads {
		if r := replies[t.ID]; r != nil {
			t = t.clone()
			t.Reply = r
			t.Comments = append(t.Comments, &Comment{
				ID: r.ID, Author: viewerName(pr), Body: r.Body, Created: r.Created, Draft: true,
			})
		}
		out = append(out, t)
	}
	for _, dt := range d.Threads {
		t := &Thread{
			ID:        dt.ID,
			Head:      dt.Head,
			Commit:    dt.Commit,
			File:      dt.File,
			Side:      dt.Side,
			Line:      dt.Line,
			StartLine: dt.StartLine,
			Anchor:    dt.Anchor,
			Resolved:  dt.Resolved,
			Draft:     dt,
			Comments: []*Comment{{
				ID: dt.ID, Author: viewerName(pr), Body: dt.Body, Created: dt.Created, Draft: true,
			}},
		}
		if s := pr.SnapshotByHead(dt.Head); s != nil {
			t.SnapshotN = s.N
		}
		out = append(out, t)
	}
	return out
}

func viewerName(pr *PR) string {
	if pr.Extra != nil && pr.Extra.Viewer != "" {
		return pr.Extra.Viewer
	}
	return "you"
}
