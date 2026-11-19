// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strings"
)

// Reviewing a pull request a commit at a time.
//
// A stack of commits is written to be read in order, each commit one idea,
// and a snapshot's whole diff throws that away. So the pull request page
// also lists a snapshot's commits, each with its own files, and each file
// and each commit can be marked reviewed on its own.
//
// Those marks have to survive the next push. Answering review rewrites the
// stack, which gives every commit a new ID, including the many it did not
// change. So a mark is not on a commit at all, but on what one file's diff
// in a commit says: the path and the lines it removes and adds, without
// line numbers, which move whenever anything above them does. A commit is
// reviewed when each of its files is. In a new snapshot, a commit whose
// files' changes are the ones already read comes up reviewed and folded
// away, however much the stack moved around it; one whose changes are new
// asks to be read again, and only for the files that differ.

// A CommitInfo is one commit of a snapshot, with its files.
type CommitInfo struct {
	*Commit
	Index   int // 1-based position in the snapshot
	Merge   bool
	Files   []*CommitFile
	Added   int
	Deleted int
}

// A CommitFile is one file changed by a commit.
type CommitFile struct {
	Path    string
	Status  byte
	OldOID  string
	NewOID  string
	FP      string // fingerprint of the change, which reviewed marks are keyed by
	Added   int
	Deleted int
	Binary  bool
}

// Char returns the one-letter status shown in file lists.
func (f *CommitFile) Char() string { return string(f.Status) }

// Identity names a commit across rewrites, for saying whether a commit
// in one snapshot is a new version of one in another: its Change-Id
// trailer if it has one, as Gerrit users' commits do, and otherwise its
// subject.
func (c *CommitInfo) Identity() string {
	lines := strings.Split(c.Message, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if id, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "Change-Id: "); ok {
			return "id:" + id
		}
	}
	return "subject:" + c.Subject
}

// Body returns the commit message without its subject line.
func (c *CommitInfo) Body() string {
	_, body, _ := strings.Cut(c.Message, "\n")
	return strings.TrimSpace(body)
}

// fingerprint identifies the change a diff makes to a file, independent
// of where in the file it is: the same edit to the same file fingerprints
// the same in every snapshot, whatever happened above or below it.
func fingerprint(path string, fd *FileDiff, oldOID, newOID string) string {
	h := sha256.New()
	h.Write([]byte(path + "\x00"))
	if fd.Binary {
		h.Write([]byte("binary\x00" + oldOID + "\x00" + newOID))
	} else {
		prevChanged := false
		for _, r := range fd.Rows {
			if r.Kind == RowEqual || r.Kind == RowSkip {
				if prevChanged {
					h.Write([]byte("@\n")) // a break between chunks
				}
				prevChanged = false
				continue
			}
			prevChanged = true
			if r.L.Num > 0 {
				h.Write([]byte("-" + r.L.Text + "\n"))
			}
			if r.R.Num > 0 {
				h.Write([]byte("+" + r.R.Text + "\n"))
			}
		}
		if oldOID == zeroOID || newOID == zeroOID {
			// Creating an empty file and deleting one are changes too.
			h.Write([]byte("\x00" + oldOID[:1] + newOID[:1]))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// Commits returns the commits of a snapshot, oldest first, with their
// files and fingerprints. A snapshot never changes, so the answer is
// kept for as long as the pull request is.
func (pr *PR) Commits(ctx context.Context, s *Snapshot) []*CommitInfo {
	pr.mu.Lock()
	cs, ok := pr.commits[s.Head]
	pr.mu.Unlock()
	if ok {
		return cs
	}
	cs = pr.loadCommits(ctx, s)
	pr.mu.Lock()
	if pr.commits == nil {
		pr.commits = map[string][]*CommitInfo{}
	}
	pr.commits[s.Head] = cs
	pr.mu.Unlock()
	return cs
}

func (pr *PR) loadCommits(ctx context.Context, s *Snapshot) []*CommitInfo {
	if s.Missing || s.Base == "" {
		return nil
	}
	log1, err := pr.Git.Log(ctx, s.Base, s.Head)
	if err != nil {
		log.Printf("%v: snapshot %d: %v", pr.Key, s.N, err)
		return nil
	}
	var out []*CommitInfo
	var oids, revs []string
	for i, c := range log1 {
		ci := &CommitInfo{Commit: c, Index: i + 1, Merge: len(c.Parents) != 1}
		out = append(out, ci)
		if ci.Merge {
			// A merge's diff against its first parent is everything the
			// other side brought, which is not this pull request's work.
			continue
		}
		fs, err := pr.Git.DiffTrees(ctx, c.Parents[0], c.SHA)
		if err != nil {
			log.Printf("%v: %v", pr.Key, err)
			continue
		}
		for _, f := range fs {
			if f.Submodule {
				continue
			}
			ci.Files = append(ci.Files, &CommitFile{Path: f.Path, Status: f.Status, OldOID: f.OldOID, NewOID: f.NewOID})
			oids = append(oids, f.OldOID, f.NewOID)
		}
		revs = append(revs, c.Parents[0], c.SHA)
	}
	if err := pr.Git.Prefetch(ctx, revs, oids); err != nil {
		log.Printf("%v: prefetch: %v", pr.Key, err)
	}
	for _, ci := range out {
		for _, f := range ci.Files {
			fd := pr.blobDiff(ctx, f.OldOID, f.NewOID)
			f.Binary = fd.Binary
			for _, r := range fd.Rows {
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
			f.FP = fingerprint(f.Path, fd, f.OldOID, f.NewOID)
			ci.Added += f.Added
			ci.Deleted += f.Deleted
		}
	}
	return out
}

// blobDiff returns the diff between two blobs, cached by their IDs.
func (pr *PR) blobDiff(ctx context.Context, oldOID, newOID string) *FileDiff {
	key := "blobs " + oldOID + " " + newOID
	pr.mu.Lock()
	fd := pr.diffs[key]
	pr.mu.Unlock()
	if fd != nil {
		return fd
	}
	fd = Diff(pr.blob(ctx, oldOID, ""), pr.blob(ctx, newOID, ""))
	pr.mu.Lock()
	if pr.diffs == nil {
		pr.diffs = map[string]*FileDiff{}
	}
	pr.diffs[key] = fd
	pr.mu.Unlock()
	return fd
}

// CommitBySHA finds a commit among a snapshot's commits.
func (pr *PR) CommitBySHA(ctx context.Context, s *Snapshot, sha string) *CommitInfo {
	for _, c := range pr.Commits(ctx, s) {
		if c.SHA == sha || len(sha) >= 7 && len(c.SHA) > len(sha) && c.SHA[:len(sha)] == sha {
			return c
		}
	}
	return nil
}

// A commitRow is a commit as the pull request page lists it.
type commitRow struct {
	*CommitInfo
	P         *prPage
	Files     []*commitFileRow
	Reviewed  bool // every file reviewed
	NReviewed int
	// Status against the snapshot before: "new" when none of the
	// commit's changes were there, "changed" when some were, and ""
	// when all of them were.
	Since   string
	SinceN  int
	Threads int
}

// A commitFileRow is a file in a commitRow.
type commitFileRow struct {
	*CommitFile
	URL      string
	Reviewed bool
	Threads  int
}

// commitRows builds the commit list for the page's target snapshot.
func (p *prPage) commitRows(ctx context.Context) []*commitRow {
	pr := p.PR
	cs := pr.Commits(ctx, p.Target)
	// What the snapshot before this one held, to say what is new.
	var prevFPs, prevIDs map[string]bool
	var prevN int
	for i := p.Target.N - 1; i >= 1; i-- {
		if s := pr.Snapshot(i); !s.Missing {
			prevFPs, prevIDs = map[string]bool{}, map[string]bool{}
			prevN = s.N
			for _, c := range pr.Commits(ctx, s) {
				prevIDs[c.Identity()] = true
				for _, f := range c.Files {
					prevFPs[f.FP] = true
				}
			}
			break
		}
	}
	var rows []*commitRow
	for _, c := range cs {
		r := &commitRow{CommitInfo: c, P: p, SinceN: prevN}
		seen := 0
		for _, f := range c.Files {
			fr := &commitFileRow{CommitFile: f, URL: p.CommitDiffURL(c.SHA, f.Path), Reviewed: p.Marks.Patches[f.FP]}
			for _, t := range p.Threads {
				if t.Commit == c.SHA && t.File == f.Path {
					fr.Threads++
				}
			}
			r.Threads += fr.Threads
			if fr.Reviewed {
				r.NReviewed++
			}
			if prevFPs[f.FP] {
				seen++
			}
			r.Files = append(r.Files, fr)
		}
		r.Reviewed = len(c.Files) > 0 && r.NReviewed == len(c.Files)
		switch {
		case prevFPs == nil || c.Merge:
		case !prevIDs[c.Identity()] && seen < len(c.Files):
			r.Since = "new"
		case seen < len(c.Files):
			r.Since = "changed"
		}
		rows = append(rows, r)
	}
	return rows
}
