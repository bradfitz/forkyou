// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Snapshot is one version of a pull request: the head commit it pointed
// at after some push, which is what Gerrit calls a patch set. Snapshots
// are numbered from 1 in the order they were pushed.
//
// Snapshots are worked out afresh from GitHub's record of the pull request
// every time it is loaded, never stored, so that everyone looking at a
// pull request sees the same numbering, and a comment made about
// "snapshot 3" means the same thing to all of them.
type Snapshot struct {
	N      int
	Head   string    // head commit
	Base   string    // merge base with the base branch: this snapshot's "parent"
	Pushed time.Time // when it was pushed, as near as GitHub says
	Pusher string    // who pushed it, when known
	Force  bool      // it arrived by force push

	Missing bool // the commit could not be fetched
	Subject string
	Commits int // commits between Base and Head
}

// ShortRev returns the abbreviated head commit.
func (s *Snapshot) ShortRev() string { return shortRev(s.Head) }

// ShortBase returns the abbreviated base commit.
func (s *Snapshot) ShortBase() string { return shortRev(s.Base) }

// A PR is everything known about a pull request, as of one load.
// It is not modified after it is built, apart from the caches inside it,
// so one PR may serve many requests at once.
type PR struct {
	Key           PRKey
	Pull          *GHPull
	Timeline      []GHTimelineEvent
	Comments      []GHReviewComment
	Reviews       []GHReview
	IssueComments []GHIssueComment
	Extra         *GQLPRExtra // nil without GraphQL
	ExtraErr      error
	Snapshots     []*Snapshot
	Threads       []*Thread // published threads, from GitHub
	FetchedAt     time.Time
	Git           *GitRepo

	mu    sync.Mutex
	blobs map[string][]byte    // by "rev:path" or OID
	diffs map[string]*FileDiff // by diffKey
}

// Latest returns the newest snapshot, or nil if there are none.
func (pr *PR) Latest() *Snapshot {
	if len(pr.Snapshots) == 0 {
		return nil
	}
	return pr.Snapshots[len(pr.Snapshots)-1]
}

// Snapshot returns the snapshot numbered n, or nil.
func (pr *PR) Snapshot(n int) *Snapshot {
	if n < 1 || n > len(pr.Snapshots) {
		return nil
	}
	return pr.Snapshots[n-1]
}

// SnapshotByHead returns the snapshot with the given head commit, or nil.
func (pr *PR) SnapshotByHead(head string) *Snapshot {
	for _, s := range pr.Snapshots {
		if s.Head == head {
			return s
		}
	}
	return nil
}

// Thread returns the published thread with the given ID, or nil.
func (pr *PR) Thread(id string) *Thread {
	for _, t := range pr.Threads {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// ---- snapshots -------------------------------------------------------------

// A pushCandidate is evidence that a commit was once the head of a PR.
type pushCandidate struct {
	sha    string
	t      time.Time
	parent string // first parent, for committed events
	kind   string // "commit", "force", "before", "reviewed", "head"
	who    string
	order  int
}

// coalesceWindow is how close together two commits must have been made
// for a run of them to be taken as one push. GitHub's timeline lists the
// commits of a regular push one by one and says nothing about which push
// brought each, so this is a guess, but a deterministic one: everybody
// looking sees the same snapshots.
const coalesceWindow = 2 * time.Minute

// buildSnapshots works out the snapshots of a pull request from GitHub's
// record of it. Its evidence:
//
//   - "committed" timeline events, one per commit now on the branch;
//   - force pushes, each naming the commit that became the head and,
//     with GraphQL, the one it replaced;
//   - reviews, each naming the head it was written against, which is
//     often the only remaining trace of a head that was force-pushed away;
//   - the pull request's current head.
//
// Runs of commits made close together, each the parent of the next, are
// taken as one push. The result is in push order, numbered from 1.
// Bases are not filled in: that needs git.
func buildSnapshots(pull *GHPull, timeline []GHTimelineEvent, reviews []GHReview, forcePushes []GQLForcePush) []*Snapshot {
	var cands []pushCandidate
	add := func(c pushCandidate) {
		if c.sha == "" {
			return
		}
		c.order = len(cands)
		cands = append(cands, c)
	}
	for _, e := range timeline {
		switch e.Event {
		case "committed":
			c := pushCandidate{sha: e.SHA, kind: "commit"}
			if e.Committer != nil {
				c.t = e.Committer.Date
			}
			if len(e.Parents) > 0 {
				c.parent = e.Parents[0].SHA
			}
			add(c)
		case "head_ref_force_pushed":
			c := pushCandidate{sha: e.CommitID, kind: "force"}
			if e.CreatedAt != nil {
				c.t = *e.CreatedAt
			}
			if e.Actor != nil {
				c.who = e.Actor.Login
			}
			add(c)
		case "reviewed":
			c := pushCandidate{sha: e.CommitID, kind: "reviewed"}
			if e.SubmittedAt != nil {
				c.t = *e.SubmittedAt
			}
			add(c)
		}
	}
	for _, r := range reviews {
		if r.State != "PENDING" && !r.SubmittedAt.IsZero() {
			add(pushCandidate{sha: r.CommitID, t: r.SubmittedAt, kind: "reviewed"})
		}
	}
	for _, fp := range forcePushes {
		who := ""
		if fp.Actor != nil {
			who = fp.Actor.Login
		}
		if fp.BeforeCommit != nil {
			// The replaced head was there just before the push.
			add(pushCandidate{sha: fp.BeforeCommit.OID, t: fp.CreatedAt.Add(-time.Millisecond), kind: "before"})
		}
		if fp.AfterCommit != nil {
			add(pushCandidate{sha: fp.AfterCommit.OID, t: fp.CreatedAt, kind: "force", who: who})
		}
	}
	if pull != nil {
		add(pushCandidate{sha: pull.Head.SHA, t: pull.UpdatedAt, kind: "head"})
	}

	// One entry per commit, at the earliest time anything mentions it,
	// except that a head is never placed before a commit it follows from.
	type entry struct {
		sha     string
		t       time.Time
		order   int
		parent  string
		pushed  time.Time
		pusher  string
		force   bool
		onlyRun bool // known only from a committed event
	}
	bySHA := map[string]*entry{}
	var entries []*entry
	for _, c := range cands {
		e := bySHA[c.sha]
		if e == nil {
			e = &entry{sha: c.sha, t: c.t, order: c.order, onlyRun: true}
			bySHA[c.sha] = e
			entries = append(entries, e)
		}
		if c.kind != "head" && (e.t.IsZero() || (!c.t.IsZero() && c.t.Before(e.t))) {
			e.t, e.order = c.t, c.order
		}
		if c.parent != "" {
			e.parent = c.parent
		}
		switch c.kind {
		case "commit", "head":
			// The current head is a head by definition, but it only stops
			// being coalescable if something else says so: a regular push
			// of three commits ends with the head and starts with two
			// commits that never were one.
			if c.kind == "head" {
				e.onlyRun = false
			}
		case "force":
			e.onlyRun = false
			e.force = true
			e.pushed = c.t
			e.pusher = c.who
		default:
			e.onlyRun = false
		}
	}
	slices.SortStableFunc(entries, func(a, b *entry) int {
		if c := a.t.Compare(b.t); c != 0 && !a.t.IsZero() && !b.t.IsZero() {
			return c
		}
		return cmp.Compare(a.order, b.order)
	})
	// The current head goes last whatever its time says: updated_at moves
	// for reasons that have nothing to do with pushes.
	if pull != nil {
		if i := slices.IndexFunc(entries, func(e *entry) bool { return e.sha == pull.Head.SHA }); i >= 0 {
			e := entries[i]
			entries = append(append(entries[:i:i], entries[i+1:]...), e)
		}
	}

	// Coalesce a run of commits pushed together: drop a commit when the
	// next is its child, made within the window, and nothing else says
	// the dropped one was ever a head.
	var kept []*entry
	for i, e := range entries {
		if i+1 < len(entries) && e.onlyRun {
			next := entries[i+1]
			if next.parent == e.sha && !next.t.IsZero() && next.t.Sub(e.t) < coalesceWindow {
				continue
			}
		}
		kept = append(kept, e)
	}

	var snaps []*Snapshot
	for i, e := range kept {
		s := &Snapshot{N: i + 1, Head: e.sha, Pushed: e.t, Pusher: e.pusher, Force: e.force}
		if !e.pushed.IsZero() {
			s.Pushed = e.pushed
		}
		snaps = append(snaps, s)
	}
	return snaps
}

// ---- threads -----------------------------------------------------------------

// A Thread is a comment thread, published or draft.
//
// Like a comment in rsc.io/cmd/review, a thread belongs to the snapshot it
// was written against, on one side of the diff: "new" is the snapshot
// itself and "old" its parent. When the thread is shown on some other
// snapshot, it is placed by looking for the text of its line near where
// the line used to be.
type Thread struct {
	ID        string // "g<id of first comment>" for GitHub, "d<n>" for a draft
	NodeID    string // GraphQL ID, for resolving
	Head      string // head of the snapshot it belongs to
	SnapshotN int
	File      string
	Side      string // "old" or "new"
	Line      int    // 0 for the file as a whole
	StartLine int    // first line of a range, or 0
	Anchor    string // the text of Line
	Resolved  bool
	Outdated  bool
	URL       string
	Comments  []*Comment

	// Draft is the thread's own draft, for a thread not yet published.
	Draft *DraftThread
	// Reply is an unpublished reply to a published thread.
	Reply *DraftReply

	// Placement for the page being drawn.
	ShowLine int
	Other    bool // written against some other snapshot
	Stale    bool // its line is gone
}

// A Comment is one comment in a thread.
type Comment struct {
	ID       string
	Author   string
	Avatar   string
	Body     string
	BodyHTML template.HTML // as GitHub rendered it; empty for drafts
	Created  time.Time
	Draft    bool
	URL      string
}

// Where describes where the thread is attached.
func (t *Thread) Where() string {
	switch {
	case t.Line == 0:
		return t.File
	case t.StartLine > 0 && t.StartLine != t.Line:
		return fmt.Sprintf("%s:%d-%d", t.File, t.StartLine, t.Line)
	}
	return fmt.Sprintf("%s:%d", t.File, t.Line)
}

// IsDraft reports whether the thread itself is unpublished.
func (t *Thread) IsDraft() bool { return t.Draft != nil }

// DraftComment returns the thread's unpublished comment, if any.
func (t *Thread) DraftComment() *Comment {
	for _, c := range t.Comments {
		if c.Draft {
			return c
		}
	}
	return nil
}

// Latest returns the time of the newest comment.
func (t *Thread) Latest() time.Time {
	var at time.Time
	for _, c := range t.Comments {
		if c.Created.After(at) {
			at = c.Created
		}
	}
	return at
}

// clone copies a thread so that a page can place it without disturbing
// the shared copy.
func (t *Thread) clone() *Thread {
	c := *t
	c.Comments = slices.Clone(t.Comments)
	return &c
}

// buildThreads groups GitHub's review comments into threads and attaches
// each to the snapshot it was written against.
func buildThreads(ctx context.Context, pr *PR) []*Thread {
	resolved := map[int64]GQLThread{}
	if pr.Extra != nil {
		for _, t := range pr.Extra.Threads {
			if len(t.Comments.Nodes) > 0 {
				resolved[t.Comments.Nodes[0].DatabaseID] = t
			}
		}
	}
	byRoot := map[int64]*Thread{}
	var threads []*Thread
	comments := slices.Clone(pr.Comments)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	rootOf := map[int64]int64{}
	for _, c := range comments {
		root := c.ID
		if c.InReplyTo != 0 {
			root = c.InReplyTo
			if r, ok := rootOf[root]; ok {
				root = r
			}
		}
		rootOf[c.ID] = root
		t := byRoot[root]
		if t == nil {
			t = &Thread{
				ID:   fmt.Sprintf("g%d", root),
				File: c.Path,
				Side: "new",
				Line: c.OriginalLine,
				URL:  c.HTMLURL,
			}
			if c.Side == "LEFT" {
				t.Side = "old"
			}
			if c.SubjectType == "file" {
				t.Line = 0
			}
			if c.OriginalStartLine > 0 && c.OriginalStartLine != c.OriginalLine {
				t.StartLine = c.OriginalStartLine
			}
			if g, ok := resolved[root]; ok {
				t.NodeID, t.Resolved, t.Outdated = g.ID, g.IsResolved, g.IsOutdated
			}
			pr.placeOnSnapshot(ctx, t, c.OriginalCommitID)
			byRoot[root] = t
			threads = append(threads, t)
		}
		t.Comments = append(t.Comments, &Comment{
			ID:       fmt.Sprintf("g%d", c.ID),
			Author:   c.User.Login,
			Avatar:   c.User.AvatarURL,
			Body:     c.Body,
			BodyHTML: template.HTML(c.BodyHTML),
			Created:  c.CreatedAt,
			URL:      c.HTMLURL,
		})
	}
	return threads
}

// placeOnSnapshot attaches a thread written against commit orig to a
// snapshot. Usually orig is a snapshot's head. When it is not, as for a
// comment made while looking at a single commit of a multi-commit push,
// the thread goes to the earliest snapshot holding that commit, with its
// anchor text read from the commit it was really written against so that
// the usual search can find where that line is in the snapshot.
func (pr *PR) placeOnSnapshot(ctx context.Context, t *Thread, orig string) {
	snap := pr.SnapshotByHead(orig)
	if snap == nil {
		for _, s := range pr.Snapshots {
			if !s.Missing && pr.Git.IsAncestor(ctx, orig, s.Head) {
				snap = s
				break
			}
		}
	}
	if snap == nil {
		snap = pr.Latest()
	}
	if snap == nil {
		return
	}
	t.Head, t.SnapshotN = snap.Head, snap.N
	if t.Line <= 0 {
		return
	}
	rev := orig
	if t.Side == "old" {
		if snap.Head == orig {
			rev = snap.Base
		} else if mb, err := pr.Git.MergeBase(ctx, orig, pr.Pull.Base.SHA); err == nil {
			rev = mb
		}
	}
	if rev == "" {
		return
	}
	lines := pr.lines(ctx, rev, t.File)
	if t.Line <= len(lines) {
		t.Anchor = lines[t.Line-1]
	}
}

// ---- file contents ---------------------------------------------------------------

// blob returns the contents of rev:path, or of a blob ID if path is "".
// Missing files read as empty.
func (pr *PR) blob(ctx context.Context, rev, path string) []byte {
	name := rev
	if path != "" {
		name = rev + ":" + path
	}
	if rev == "" || rev == zeroOID {
		return nil
	}
	pr.mu.Lock()
	data, ok := pr.blobs[name]
	pr.mu.Unlock()
	if ok {
		return data
	}
	data, err := pr.Git.ReadBlob(ctx, name)
	if err != nil {
		log.Printf("%v: reading %s: %v", pr.Key, name, err)
	}
	pr.mu.Lock()
	if pr.blobs == nil {
		pr.blobs = map[string][]byte{}
	}
	pr.blobs[name] = data
	pr.mu.Unlock()
	return data
}

// lines returns the lines of rev:path.
func (pr *PR) lines(ctx context.Context, rev, path string) []string {
	l, _ := splitLines(pr.blob(ctx, rev, path))
	return l
}

// prefetchPaths fetches, in one round trip, the blobs for paths in each
// of revs, so that reading them afterward needs no network.
func (pr *PR) prefetchPaths(ctx context.Context, revs []string, paths []string) {
	if len(paths) == 0 {
		return
	}
	var oids []string
	seen := map[string]bool{}
	for _, rev := range revs {
		if rev == "" || seen[rev] {
			continue
		}
		seen[rev] = true
		m, err := pr.Git.BlobOIDs(ctx, rev, paths)
		if err != nil {
			log.Printf("%v: %v", pr.Key, err)
			continue
		}
		for _, o := range m {
			oids = append(oids, o)
		}
	}
	if err := pr.Git.Prefetch(ctx, revs, oids); err != nil {
		log.Printf("%v: prefetch: %v", pr.Key, err)
	}
}

// ---- loading ---------------------------------------------------------------------

// loadPR fetches a pull request from GitHub and builds its snapshots and
// threads. The REST requests go out in parallel, and each is a cheap 304
// when nothing has changed since the last load.
func (s *server) loadPR(ctx context.Context, k PRKey) (*PR, error) {
	base := fmt.Sprintf("/repos/%s/%s", k.Owner, k.Repo)
	pr := &PR{Key: k, FetchedAt: time.Now(), Git: s.gitRepo(k.Owner, k.Repo)}
	var wg sync.WaitGroup
	errs := make([]error, 6)
	wg.Go(func() {
		pr.Pull = new(GHPull)
		errs[0] = s.gh.Get(ctx, fmt.Sprintf("%s/pulls/%d", base, k.Number), pr.Pull)
	})
	wg.Go(func() {
		pr.Timeline, errs[1] = GetAll[GHTimelineEvent](ctx, s.gh, fmt.Sprintf("%s/issues/%d/timeline?per_page=100", base, k.Number))
	})
	wg.Go(func() {
		pr.Comments, errs[2] = GetAll[GHReviewComment](ctx, s.gh, fmt.Sprintf("%s/pulls/%d/comments?per_page=100", base, k.Number))
	})
	wg.Go(func() {
		pr.Reviews, errs[3] = GetAll[GHReview](ctx, s.gh, fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", base, k.Number))
	})
	wg.Go(func() {
		pr.IssueComments, errs[4] = GetAll[GHIssueComment](ctx, s.gh, fmt.Sprintf("%s/issues/%d/comments?per_page=100", base, k.Number))
	})
	if s.gh.Token != "" {
		wg.Go(func() {
			pr.Extra, pr.ExtraErr = s.gh.PRExtra(ctx, k.Owner, k.Repo, k.Number)
			if pr.ExtraErr != nil {
				log.Printf("%v: graphql: %v", k, pr.ExtraErr)
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	var fps []GQLForcePush
	if pr.Extra != nil {
		fps = pr.Extra.ForcePushes
	}
	pr.Snapshots = buildSnapshots(pr.Pull, pr.Timeline, pr.Reviews, fps)

	// Fetch every snapshot's head, and the base branch, in one go.
	var heads []string
	for _, sn := range pr.Snapshots {
		heads = append(heads, sn.Head)
	}
	var refspecs []string
	if !pr.Git.HasCommit(ctx, pr.Pull.Head.SHA) {
		// A pull request from a fork has its head under this name too.
		refspecs = append(refspecs, fmt.Sprintf("refs/pull/%d/head", k.Number))
	}
	if err := pr.Git.FetchCommits(ctx, append(heads, pr.Pull.Base.SHA), refspecs...); err != nil {
		log.Printf("%v: fetch: %v", k, err)
		if r := pr.Pull.Head.Repo; r != nil && r.CloneURL != "" && r.FullName != k.Owner+"/"+k.Repo {
			// The fork may still have what the base repository lost.
			for _, h := range heads {
				if !pr.Git.HasCommit(ctx, h) {
					pr.Git.run(ctx, "fetch", "-q", "--no-tags", "--no-write-fetch-head", "--filter=blob:none", r.CloneURL, h)
				}
			}
		}
	}
	for _, sn := range pr.Snapshots {
		if !pr.Git.HasCommit(ctx, sn.Head) {
			sn.Missing = true
			continue
		}
		mb, err := pr.Git.MergeBase(ctx, sn.Head, pr.Pull.Base.SHA)
		if err != nil {
			log.Printf("%v: snapshot %d: %v", k, sn.N, err)
			sn.Missing = true
			continue
		}
		sn.Base = mb
		if cs, err := pr.Git.Log(ctx, mb, sn.Head); err == nil {
			sn.Commits = len(cs)
			if len(cs) > 0 {
				sn.Subject = cs[len(cs)-1].Subject
			}
		}
	}
	// A snapshot that could not be fetched has nothing to show, and keeping
	// it would only offer a page that fails. It keeps its number, though,
	// by staying in the list marked missing, so that numbers do not shift.

	// Threads need the text of the lines they were written on.
	type revPath struct{ rev, path string }
	var paths []string
	var revs []string
	for _, c := range pr.Comments {
		paths = append(paths, c.Path)
		revs = append(revs, c.OriginalCommitID)
	}
	for _, sn := range pr.Snapshots {
		revs = append(revs, sn.Base)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	pr.prefetchPaths(ctx, compactStrings(revs), paths)
	pr.Threads = buildThreads(ctx, pr)
	return pr, nil
}

func compactStrings(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	s = slices.Compact(s)
	if len(s) > 0 && s[0] == "" {
		s = s[1:]
	}
	return s
}

// ---- conversation ------------------------------------------------------------------

// A ConvItem is one entry in a pull request's conversation: the opening
// description, a comment on the pull request as a whole, a review, or a
// push.
type ConvItem struct {
	Kind     string // "comment", "review", "push"
	Author   string
	Avatar   string
	When     time.Time
	BodyHTML template.HTML
	State    string // for reviews
	URL      string
	Snapshot *Snapshot // for pushes, and the snapshot a review was of
	Threads  int       // for reviews: inline comments it carried
}

// Conversation returns the pull request's conversation in time order.
func (pr *PR) Conversation() []*ConvItem {
	var items []*ConvItem
	for _, c := range pr.IssueComments {
		items = append(items, &ConvItem{
			Kind: "comment", Author: c.User.Login, Avatar: c.User.AvatarURL,
			When: c.CreatedAt, BodyHTML: template.HTML(c.BodyHTML), URL: c.HTMLURL,
		})
	}
	inline := map[int64]int{}
	for _, c := range pr.Comments {
		inline[c.ReviewID]++
	}
	for _, r := range pr.Reviews {
		if r.State == "PENDING" {
			continue
		}
		// A review that is nothing but a reply to a thread is GitHub's way
		// of recording that reply. The thread shows it; listing it again
		// here would say the same thing twice.
		if r.State == "COMMENTED" && strings.TrimSpace(r.Body) == "" {
			continue
		}
		items = append(items, &ConvItem{
			Kind: "review", Author: r.User.Login, Avatar: r.User.AvatarURL,
			When: r.SubmittedAt, BodyHTML: template.HTML(r.BodyHTML), State: r.State,
			URL: r.HTMLURL, Snapshot: pr.SnapshotByHead(r.CommitID), Threads: inline[r.ID],
		})
	}
	for _, s := range pr.Snapshots {
		items = append(items, &ConvItem{Kind: "push", Author: s.Pusher, When: s.Pushed, Snapshot: s})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].When.Before(items[j].When) })
	return items
}
