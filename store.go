// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// A Store keeps the state that is this program's own rather than
// GitHub's: unpublished drafts, reviewed marks, and preferences. It is
// a tree of small JSON files, one directory per pull request:
//
//	<dir>/prefs.json
//	<dir>/recent.json
//	<dir>/pr/<owner>/<repo>/<number>/drafts.json
//	<dir>/pr/<owner>/<repo>/<number>/reviewed.json
//
// Everything is read and written whole, under one lock. The files are
// tiny and written only when the reviewer does something, so there is
// nothing here a database would make faster.
type Store struct {
	Dir string
	mu  sync.Mutex
}

// A PRKey names a pull request.
type PRKey struct {
	Owner, Repo string
	Number      int
}

func (k PRKey) String() string { return fmt.Sprintf("%s/%s#%d", k.Owner, k.Repo, k.Number) }

// URL is the path of the pull request's page, which is the same as on
// github.com so that swapping the host name is all it takes to get here.
func (k PRKey) URL() string { return fmt.Sprintf("/%s/%s/pull/%d", k.Owner, k.Repo, k.Number) }

func (s *Store) prDir(k PRKey) string {
	return filepath.Join(s.Dir, "pr", k.Owner, k.Repo, strconv.Itoa(k.Number))
}

func (s *Store) load(name string, v any) error {
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (s *Store) save(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "\t")
	if err != nil {
		return err
	}
	return writeFileAtomic(name, append(data, '\n'))
}

// ---- drafts ----------------------------------------------------------------

// Drafts are the comments written and not yet published, in Gerrit's
// manner: a review is composed as a whole and then sent as one GitHub
// review, rather than dribbled out a remark at a time.
type Drafts struct {
	NextID  int            `json:"next_id"`
	Threads []*DraftThread `json:"threads,omitempty"`
	Replies []*DraftReply  `json:"replies,omitempty"`
	// Message is the draft of the review's own body.
	Message string `json:"message,omitempty"`
}

// A DraftThread is a new comment thread that has not been published.
type DraftThread struct {
	ID        string    `json:"id"`   // "d<n>"
	Head      string    `json:"head"` // head commit of the snapshot it was written against
	File      string    `json:"file"`
	Side      string    `json:"side"`                 // "old" (the snapshot's parent) or "new"
	Line      int       `json:"line,omitempty"`       // 0 for a comment on the file as a whole
	StartLine int       `json:"start_line,omitempty"` // first line of a range, or 0
	Anchor    string    `json:"anchor,omitempty"`     // text of Line when written
	Body      string    `json:"body"`
	Resolved  bool      `json:"resolved,omitempty"`
	Created   time.Time `json:"created"`
}

// A DraftReply is an unpublished reply to a thread already on GitHub.
type DraftReply struct {
	ID       string    `json:"id"`
	ThreadID string    `json:"thread"` // Thread.ID of the GitHub thread
	Body     string    `json:"body"`
	Resolve  bool      `json:"resolve,omitempty"` // resolve the thread once this is published
	Created  time.Time `json:"created"`
}

// Count returns the number of draft comments.
func (d *Drafts) Count() int { return len(d.Threads) + len(d.Replies) }

func (d *Drafts) newID() string {
	d.NextID++
	return "d" + strconv.Itoa(d.NextID)
}

// Drafts returns the drafts for a pull request.
func (s *Store) Drafts(k PRKey) (*Drafts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := new(Drafts)
	err := s.load(filepath.Join(s.prDir(k), "drafts.json"), d)
	return d, err
}

// UpdateDrafts calls f with the drafts for a pull request and saves the
// result if f succeeds.
func (s *Store) UpdateDrafts(k PRKey, f func(*Drafts) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := filepath.Join(s.prDir(k), "drafts.json")
	d := new(Drafts)
	if err := s.load(name, d); err != nil {
		return err
	}
	if err := f(d); err != nil {
		return err
	}
	return s.save(name, d)
}

// ---- reviewed marks ----------------------------------------------------------

// Reviewed records what the reviewer has read. Marks are keyed by the
// head commit of a snapshot rather than its number, since the commit is
// what was read.
type Reviewed struct {
	// Files maps a snapshot's head to the set of files marked reviewed in it.
	Files map[string]map[string]bool `json:"files,omitempty"`
	// Snapshots is the set of snapshot heads marked reviewed.
	Snapshots map[string]bool `json:"snapshots,omitempty"`
}

// Reviewed returns the reviewed marks for a pull request.
func (s *Store) Reviewed(k PRKey) (*Reviewed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := new(Reviewed)
	err := s.load(filepath.Join(s.prDir(k), "reviewed.json"), r)
	return r, err
}

// UpdateReviewed calls f with the reviewed marks and saves them.
func (s *Store) UpdateReviewed(k PRKey, f func(*Reviewed)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := filepath.Join(s.prDir(k), "reviewed.json")
	r := new(Reviewed)
	if err := s.load(name, r); err != nil {
		return err
	}
	if r.Files == nil {
		r.Files = map[string]map[string]bool{}
	}
	if r.Snapshots == nil {
		r.Snapshots = map[string]bool{}
	}
	f(r)
	return s.save(name, r)
}

// ---- preferences and history --------------------------------------------------

// Prefs are the diff display preferences.
type Prefs struct {
	Context int  `json:"context"`
	TabSize int  `json:"tabsize"`
	Unified bool `json:"unified"`
}

// Prefs returns the saved preferences, with defaults filled in.
func (s *Store) Prefs() Prefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := Prefs{Context: 10, TabSize: 8}
	s.load(filepath.Join(s.Dir, "prefs.json"), &p)
	return p
}

// SetPrefs saves the preferences.
func (s *Store) SetPrefs(p Prefs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(filepath.Join(s.Dir, "prefs.json"), p)
}

// A RecentPR is a pull request opened here lately.
type RecentPR struct {
	Key    PRKey     `json:"key"`
	Title  string    `json:"title"`
	Author string    `json:"author"`
	State  string    `json:"state"`
	Viewed time.Time `json:"viewed"`
}

// URL returns the pull request's page.
func (r *RecentPR) URL() string { return r.Key.URL() }

const maxRecent = 50

// NoteRecent records that a pull request was viewed.
func (s *Store) NoteRecent(r RecentPR) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := filepath.Join(s.Dir, "recent.json")
	var list []RecentPR
	s.load(name, &list)
	out := []RecentPR{r}
	for _, x := range list {
		if x.Key != r.Key {
			out = append(out, x)
		}
	}
	if len(out) > maxRecent {
		out = out[:maxRecent]
	}
	return s.save(name, out)
}

// Recent returns the pull requests viewed most recently, newest first.
func (s *Store) Recent() []RecentPR {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []RecentPR
	s.load(filepath.Join(s.Dir, "recent.json"), &list)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Viewed.After(list[j].Viewed) })
	return list
}
