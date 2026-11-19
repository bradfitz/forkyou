// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A GitRepo is a local bare, blob-less clone of a GitHub repository,
// used to read the code of every snapshot of a pull request.
//
// The GitHub API could serve file contents too, but one request per file
// per snapshot is slow, and diffing two snapshots against their own
// parents needs four versions of every file. A partial clone holds every
// commit and tree but fetches file contents only when asked, so the first
// clone is cheap even for a large repository, and GitHub serves commits
// by ID even after a force push has dropped them from every branch.
type GitRepo struct {
	Dir    string // the bare repository
	Remote string // URL to fetch from
	Token  string // GitHub token for HTTPS, or ""

	fetchMu sync.Mutex
	initErr error
	inited  bool

	catMu  sync.Mutex
	cat    *exec.Cmd
	catIn  io.WriteCloser
	catOut *bufio.Reader
}

// env returns the environment for git commands. A token is passed by
// configuration in the environment rather than on the command line, so
// that it does not show up in a process listing.
func (r *GitRepo) env() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_DIR="+r.Dir)
	if r.Token != "" && strings.HasPrefix(r.Remote, "https://") {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + r.Token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
		)
	}
	return env
}

func (r *GitRepo) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = r.env()
	return cmd
}

// run runs git and returns its standard output.
func (r *GitRepo) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := r.cmd(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// noLazy runs git with lazy fetching of missing objects turned off, for
// asking what is present without the asking fetching it.
func (r *GitRepo) noLazy(ctx context.Context, args ...string) *exec.Cmd {
	cmd := r.cmd(ctx, args...)
	cmd.Env = append(cmd.Env, "GIT_NO_LAZY_FETCH=1")
	return cmd
}

// Init makes sure the bare repository exists and is configured to fetch
// from Remote as a blob-less partial clone.
func (r *GitRepo) Init(ctx context.Context) error {
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	if r.inited {
		return r.initErr
	}
	r.inited = true
	if _, err := os.Stat(r.Dir + "/HEAD"); err != nil {
		if err := os.MkdirAll(r.Dir, 0o700); err != nil {
			r.initErr = err
			return err
		}
		if _, err := r.run(ctx, "init", "--bare", "-q"); err != nil {
			r.initErr = err
			return err
		}
	}
	for _, kv := range [][2]string{
		{"remote.origin.url", r.Remote},
		{"remote.origin.promisor", "true"},
		{"remote.origin.partialclonefilter", "blob:none"},
		{"extensions.partialclone", "origin"},
		{"core.repositoryformatversion", "1"},
		// Nothing but this program uses the repository, and an automatic
		// gc in the middle of a page load is a long stall for nothing.
		{"gc.auto", "0"},
		{"fetch.writeCommitGraph", "true"},
	} {
		if _, err := r.run(ctx, "config", kv[0], kv[1]); err != nil {
			r.initErr = err
			return err
		}
	}
	return nil
}

// HasCommit reports whether the commit is present locally.
func (r *GitRepo) HasCommit(ctx context.Context, sha string) bool {
	return r.noLazy(ctx, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

// FetchCommits makes sure every named commit is present, fetching the
// missing ones (and their history) in a single round trip. Extra refspecs,
// such as the base branch, are fetched along with them. It fetches the
// commits one at a time if fetching them together fails, so that one
// commit GitHub no longer has does not stop the rest.
func (r *GitRepo) FetchCommits(ctx context.Context, shas []string, refspecs ...string) error {
	if err := r.Init(ctx); err != nil {
		return err
	}
	var missing []string
	seen := map[string]bool{}
	for _, s := range shas {
		if s != "" && !seen[s] && !r.HasCommit(ctx, s) {
			seen[s] = true
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 && len(refspecs) == 0 {
		return nil
	}
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	args := []string{"fetch", "-q", "--no-tags", "--no-write-fetch-head", "--filter=blob:none", "origin"}
	err := runFetch(r, ctx, append(append(args[:len(args):len(args)], refspecs...), missing...))
	if err == nil || len(missing) < 2 {
		return err
	}
	var errs []error
	for _, s := range missing {
		if e := runFetch(r, ctx, append(args[:len(args):len(args)], s)); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

func runFetch(r *GitRepo, ctx context.Context, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err := r.run(ctx, args...)
	return err
}

// Prefetch fetches whichever of the named blobs are missing, in a single
// round trip. Left alone, git would fetch each one separately the moment
// it was read, which for a diff of a hundred files is a hundred round trips.
//
// The blobs must be in the trees of the named commits. That is how the
// missing ones are found: git will not say whether a single blob is
// present without trying to fetch it, but it will list the missing
// objects of a whole tree, which is quick even for a large one.
func (r *GitRepo) Prefetch(ctx context.Context, revs, oids []string) error {
	want := map[string]bool{}
	for _, o := range oids {
		if o != "" && o != zeroOID {
			want[o] = true
		}
	}
	revs = compactStrings(revs)
	if len(want) == 0 || len(revs) == 0 {
		return nil
	}
	cmd := r.noLazy(ctx, "rev-list", "--objects", "--no-walk", "--missing=print", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(revs, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git rev-list --missing=print: %v", err)
	}
	var missing []string
	for _, line := range strings.Split(string(out), "\n") {
		if oid, ok := strings.CutPrefix(line, "?"); ok && want[oid] {
			missing = append(missing, oid)
			delete(want, oid)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	// This is the fetch git itself makes to fill in a partial clone.
	for len(missing) > 0 {
		n := min(len(missing), 1000) // keep the command line a sane length
		args := []string{"-c", "fetch.negotiationAlgorithm=noop", "fetch", "-q", "--no-tags",
			"--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none", "origin"}
		if err := runFetch(r, ctx, append(args, missing[:n]...)); err != nil {
			return err
		}
		missing = missing[n:]
	}
	return nil
}

const zeroOID = "0000000000000000000000000000000000000000"

// MergeBase returns the best common ancestor of a and b.
func (r *GitRepo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := r.run(ctx, "merge-base", a, b)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *GitRepo) IsAncestor(ctx context.Context, a, b string) bool {
	return r.cmd(ctx, "merge-base", "--is-ancestor", a, b).Run() == nil
}

// A FileChange is one file differing between two trees.
type FileChange struct {
	Path      string
	Status    byte // 'A', 'D', 'M', or 'T'
	OldOID    string
	NewOID    string
	OldMode   string
	NewMode   string
	Binary    bool // set by callers that have looked
	Submodule bool
}

// DiffTrees lists the files that differ between the trees of commits a
// and b. It reads only trees, never file contents.
func (r *GitRepo) DiffTrees(ctx context.Context, a, b string) ([]FileChange, error) {
	out, err := r.run(ctx, "diff-tree", "-r", "-z", "--no-renames", "--no-abbrev", a, b)
	if err != nil {
		return nil, err
	}
	// Each entry is ":oldmode newmode oldoid newoid status\0path\0".
	var files []FileChange
	f := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(f[i], ":"))
		if len(meta) < 5 {
			break
		}
		fc := FileChange{
			Path:    f[i+1],
			OldMode: meta[0],
			NewMode: meta[1],
			OldOID:  meta[2],
			NewOID:  meta[3],
			Status:  meta[4][0],
		}
		fc.Submodule = fc.OldMode == "160000" || fc.NewMode == "160000"
		files = append(files, fc)
	}
	return files, nil
}

// BlobOIDs returns the blob IDs of the named paths in commit rev,
// leaving out paths that are not files there.
func (r *GitRepo) BlobOIDs(ctx context.Context, rev string, paths []string) (map[string]string, error) {
	m := map[string]string{}
	if len(paths) == 0 {
		return m, nil
	}
	args := append([]string{"ls-tree", "-z", "--full-tree", rev, "--"}, paths...)
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	for _, ent := range strings.Split(string(out), "\x00") {
		meta, path, ok := strings.Cut(ent, "\t")
		if !ok {
			continue
		}
		fs := strings.Fields(meta)
		if len(fs) == 3 && fs[1] == "blob" {
			m[path] = fs[2]
		}
	}
	return m, nil
}

// ReadBlob returns the contents of a blob, or of rev:path.
// It returns nil, nil for a name that does not exist, which is how an
// added or deleted file reads on the side where it is absent.
func (r *GitRepo) ReadBlob(ctx context.Context, name string) ([]byte, error) {
	if name == "" || name == zeroOID {
		return nil, nil
	}
	r.catMu.Lock()
	defer r.catMu.Unlock()
	if r.cat == nil {
		// Not tied to ctx: the process outlives any one request.
		cmd := exec.Command("git", "cat-file", "--batch")
		cmd.Env = r.env()
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		r.cat, r.catIn, r.catOut = cmd, in, bufio.NewReaderSize(out, 64<<10)
	}
	if _, err := io.WriteString(r.catIn, name+"\n"); err != nil {
		r.closeCat()
		return nil, err
	}
	hdr, err := r.catOut.ReadString('\n')
	if err != nil {
		r.closeCat()
		return nil, err
	}
	hdr = strings.TrimSuffix(hdr, "\n")
	if strings.HasSuffix(hdr, " missing") || strings.HasSuffix(hdr, " ambiguous") {
		return nil, nil
	}
	f := strings.Fields(hdr)
	if len(f) != 3 {
		r.closeCat()
		return nil, fmt.Errorf("git cat-file: unexpected header %q", hdr)
	}
	if f[1] != "blob" {
		// A tree or a submodule commit: not a file to show.
		n, _ := strconv.Atoi(f[2])
		io.CopyN(io.Discard, r.catOut, int64(n)+1)
		return nil, nil
	}
	n, err := strconv.Atoi(f[2])
	if err != nil {
		r.closeCat()
		return nil, err
	}
	data := make([]byte, n+1)
	if _, err := io.ReadFull(r.catOut, data); err != nil {
		r.closeCat()
		return nil, err
	}
	return data[:n], nil
}

func (r *GitRepo) closeCat() {
	if r.cat != nil {
		r.catIn.Close()
		r.cat.Wait()
		r.cat = nil
	}
}

// A Commit is one commit in a snapshot's range.
type Commit struct {
	SHA     string
	Parents []string
	Author  string
	Date    time.Time
	Subject string
	Message string
}

// ShortSHA returns the abbreviated commit ID.
func (c *Commit) ShortSHA() string { return shortRev(c.SHA) }

// Log returns the commits reachable from head but not from base, oldest first.
func (r *GitRepo) Log(ctx context.Context, base, head string) ([]*Commit, error) {
	out, err := r.run(ctx, "log", "--reverse", "-z", "--format=%H%x00%P%x00%an%x00%aI%x00%B", base+".."+head)
	if err != nil {
		return nil, err
	}
	f := strings.Split(string(out), "\x00")
	var cs []*Commit
	for i := 0; i+4 < len(f); i += 5 {
		t, _ := time.Parse(time.RFC3339, f[i+3])
		msg := strings.TrimSpace(f[i+4])
		subj, _, _ := strings.Cut(msg, "\n")
		cs = append(cs, &Commit{
			SHA:     strings.TrimLeft(f[i], "\n"),
			Parents: strings.Fields(f[i+1]),
			Author:  f[i+2],
			Date:    t,
			Subject: subj,
			Message: msg,
		})
	}
	return cs, nil
}

// RangeDiff runs git range-diff comparing the commits base1..head1 with
// base2..head2, and returns its uncolored output.
func (r *GitRepo) RangeDiff(ctx context.Context, base1, head1, base2, head2 string) (string, error) {
	// range-diff reads every blob of every commit in both ranges, so
	// fetch them all first in one go rather than one at a time.
	var oids, revs []string
	for _, rng := range [][2]string{{base1, head1}, {base2, head2}} {
		cs, err := r.Log(ctx, rng[0], rng[1])
		if err != nil {
			return "", err
		}
		for _, c := range cs {
			if len(c.Parents) != 1 {
				continue
			}
			fs, err := r.DiffTrees(ctx, c.Parents[0], c.SHA)
			if err != nil {
				return "", err
			}
			for _, f := range fs {
				oids = append(oids, f.OldOID, f.NewOID)
			}
			revs = append(revs, c.Parents[0], c.SHA)
		}
	}
	if err := r.Prefetch(ctx, revs, oids); err != nil {
		return "", err
	}
	// The default creation factor of 60 refuses to pair a commit with its
	// new version once review has changed it much, which is exactly the
	// pair a reviewer wants to see.
	out, err := r.run(ctx, "range-diff", "--no-color", "--creation-factor=100", base1+".."+head1, base2+".."+head2)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
