// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fakeGitHub serves enough of GitHub's REST and GraphQL APIs for one
// repository with one pull request, backed by a real git repository that
// the server under test clones from. It enforces GitHub's rule that a line
// comment must sit in a hunk of the pull request's diff, so that tests
// find out if the server ever asks for something GitHub would refuse.
type fakeGitHub struct {
	t      *testing.T
	dir    string // holds origin/ (bare) and work/ (a clone to commit in)
	origin string
	work   string
	srv    *httptest.Server

	mu          sync.Mutex
	owner, repo string
	number      int
	author      string
	viewer      string
	base        string // base branch name
	head        string // head branch name
	timeline    []GHTimelineEvent
	forcePushes []GQLForcePush
	comments    []GHReviewComment
	reviews     []GHReview
	threads     map[int64]*fakeThread // by root comment ID
	nextID      int64
	pending     *fakePending
	created     time.Time
	clock       time.Time

	// Errors to inject: GraphQL calls whose query contains the key fail.
	failGraphQL map[string]string
}

type fakeThread struct {
	nodeID   string
	root     int64
	resolved bool
}

type fakePending struct {
	id     string
	commit string
	adds   []GHReviewComment
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	dir := t.TempDir()
	f := &fakeGitHub{
		t:       t,
		dir:     dir,
		owner:   "acme",
		repo:    "widget",
		number:  1,
		author:  "alice",
		viewer:  "bob",
		base:    "main",
		head:    "feature",
		threads: map[int64]*fakeThread{},
		nextID:  100,
		created: time.Now().Add(-48 * time.Hour),
		clock:   time.Now().Add(-47 * time.Hour),
	}
	f.origin = filepath.Join(dir, "origin", f.owner, f.repo+".git")
	f.work = filepath.Join(dir, "work")
	f.git("", "init", "-q", "--bare", "-b", "main", f.origin)
	f.origin, _ = filepath.Abs(f.origin)
	// Let the server under test fetch any commit by ID, as GitHub does.
	f.git(f.origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	f.git(f.origin, "config", "uploadpack.allowFilter", "true")
	f.git("", "clone", "-q", f.origin, f.work)
	f.git(f.work, "config", "user.name", "Test")
	f.git(f.work, "config", "user.email", "test@example.com")
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

// git runs git in dir and returns its trimmed output.
func (f *fakeGitHub) git(dir string, args ...string) string {
	f.t.Helper()
	if dir != "" && dir == f.origin {
		args = append([]string{"--git-dir", f.origin}, args...)
		dir = ""
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+f.clock.Format(time.RFC3339), "GIT_COMMITTER_DATE="+f.clock.Format(time.RFC3339))
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// tick moves the fake clock on, so that pushes have distinct times far
// enough apart not to be taken as one push.
func (f *fakeGitHub) tick() time.Time {
	f.clock = f.clock.Add(10 * time.Minute)
	return f.clock
}

// write writes files in the work tree and commits them.
func (f *fakeGitHub) commit(msg string, files map[string]string) string {
	f.t.Helper()
	f.tick()
	for name, content := range files {
		p := filepath.Join(f.work, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
		f.git(f.work, "add", name)
	}
	f.git(f.work, "commit", "-q", "-m", msg)
	return f.git(f.work, "rev-parse", "HEAD")
}

// pushBase pushes the work tree's HEAD to the base branch.
func (f *fakeGitHub) pushBase() {
	f.git(f.work, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+f.base)
}

// pushHead pushes the work tree's HEAD as the pull request's head and
// records the push in the timeline, as GitHub would.
func (f *fakeGitHub) pushHead(force bool) string {
	f.t.Helper()
	sha := f.git(f.work, "rev-parse", "HEAD")
	f.mu.Lock()
	defer f.mu.Unlock()
	old := ""
	if out, err := exec.Command("git", "--git-dir", f.origin, "rev-parse", "--verify", "-q", "refs/heads/"+f.head).Output(); err == nil {
		old = strings.TrimSpace(string(out))
	}
	f.git(f.work, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+f.head, "HEAD:refs/pull/1/head")
	when := f.tick()
	if force {
		// GitHub drops the committed events of commits no longer on the
		// branch, and records the force push.
		var kept []GHTimelineEvent
		for _, e := range f.timeline {
			if e.Event == "committed" && !f.isAncestor(e.SHA, sha) {
				continue
			}
			kept = append(kept, e)
		}
		f.timeline = kept
	}
	// A committed event for each new commit.
	mb := f.git(f.origin, "merge-base", sha, "refs/heads/"+f.base)
	revs := strings.Fields(f.git(f.origin, "rev-list", "--reverse", mb+".."+sha))
	have := map[string]bool{}
	for _, e := range f.timeline {
		have[e.SHA] = true
	}
	for _, r := range revs {
		if have[r] {
			continue
		}
		parent := f.git(f.origin, "rev-parse", r+"^")
		date, _ := time.Parse(time.RFC3339, f.git(f.origin, "log", "-1", "--format=%cI", r))
		f.timeline = append(f.timeline, committed(r, parent, date))
	}
	if force {
		f.timeline = append(f.timeline, forced(sha, when))
		f.forcePushes = append(f.forcePushes, GQLForcePush{
			CreatedAt:    when,
			Actor:        &struct{ Login string }{f.author},
			BeforeCommit: &struct{ OID string }{old},
			AfterCommit:  &struct{ OID string }{sha},
		})
	}
	return sha
}

func (f *fakeGitHub) isAncestor(a, b string) bool {
	return exec.Command("git", "--git-dir", f.origin, "merge-base", "--is-ancestor", a, b).Run() == nil
}

func (f *fakeGitHub) headSHA() string {
	return strings.TrimSpace(f.git(f.origin, "rev-parse", "refs/heads/"+f.head))
}

func (f *fakeGitHub) baseSHA() string {
	return strings.TrimSpace(f.git(f.origin, "rev-parse", "refs/heads/"+f.base))
}

// addComment adds a published review comment, as someone else made on GitHub.
func (f *fakeGitHub) addComment(c GHReviewComment) GHReviewComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addCommentLocked(c)
}

func (f *fakeGitHub) addCommentLocked(c GHReviewComment) GHReviewComment {
	f.nextID++
	c.ID = f.nextID
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	if c.SubjectType == "" {
		c.SubjectType = "line"
	}
	if c.Side == "" {
		c.Side = "RIGHT"
	}
	c.BodyHTML = "<p>" + c.Body + "</p>"
	c.HTMLURL = fmt.Sprintf("https://github.com/%s/%s/pull/1#discussion_r%d", f.owner, f.repo, c.ID)
	f.comments = append(f.comments, c)
	if c.InReplyTo == 0 {
		f.threads[c.ID] = &fakeThread{nodeID: fmt.Sprintf("PRRT_%d", c.ID), root: c.ID}
	}
	return c
}

func (f *fakeGitHub) threadByNode(id string) *fakeThread {
	for _, t := range f.threads {
		if t.nodeID == id {
			return t
		}
	}
	return nil
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.URL.Path == "/graphql" {
		f.graphql(w, req)
		return
	}
	pre := fmt.Sprintf("/repos/%s/%s", f.owner, f.repo)
	var v any
	switch p := req.URL.Path; p {
	case "/user":
		v = GHUser{Login: f.viewer}
	case pre + "/pulls/1":
		v = f.pull()
	case pre + "/pulls":
		v = []*GHPull{f.pull()}
	case pre + "/issues/1/timeline":
		v = f.timeline
	case pre + "/pulls/1/comments":
		v = f.comments
	case pre + "/pulls/1/reviews":
		v = f.reviews
	case pre + "/issues/1/comments":
		v = []GHIssueComment{}
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) pull() *GHPull {
	p := &GHPull{
		Number:    1,
		NodeID:    "PR_1",
		Title:     "Make the widget better",
		Body:      "It is better.",
		BodyHTML:  "<p>It is better.</p>",
		State:     "open",
		User:      GHUser{Login: f.author},
		CreatedAt: f.created,
		UpdatedAt: f.clock,
		HTMLURL:   "https://github.com/acme/widget/pull/1",
	}
	p.Head.SHA, p.Head.Ref, p.Head.Label = f.headSHA(), f.head, "alice:"+f.head
	p.Base.SHA, p.Base.Ref = f.baseSHA(), f.base
	return p
}

var aliasRE = regexp.MustCompile(`(\w+): (\w+)\(input: \$(\w+)\)`)

func (f *fakeGitHub) graphql(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Query     string                     `json:"query"`
		Variables map[string]json.RawMessage `json:"variables"`
	}
	data, _ := io.ReadAll(req.Body)
	if err := json.Unmarshal(data, &in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for k, msg := range f.failGraphQL {
		if strings.Contains(in.Query, k) {
			json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": msg}}})
			return
		}
	}
	out := map[string]any{}
	var errs []map[string]string
	fail := func(format string, args ...any) {
		errs = append(errs, map[string]string{"message": fmt.Sprintf(format, args...)})
	}
	switch {
	case strings.Contains(in.Query, "reviewThreads"):
		var nodes []map[string]any
		for _, c := range f.comments {
			t := f.threads[c.ID]
			if t == nil {
				continue
			}
			nodes = append(nodes, map[string]any{
				"id": t.nodeID, "isResolved": t.resolved, "isOutdated": false,
				"comments": map[string]any{"nodes": []map[string]any{{"databaseId": t.root}}},
			})
		}
		var pend []map[string]any
		if f.pending != nil {
			pend = append(pend, map[string]any{"id": f.pending.id, "viewerDidAuthor": true})
		}
		out["viewer"] = map[string]any{"login": f.viewer}
		out["repository"] = map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes},
			"timelineItems": map[string]any{"nodes": f.forcePushes},
			"reviews":       map[string]any{"nodes": pend},
		}}
	case strings.Contains(in.Query, "{ viewer { login } }"):
		out["viewer"] = map[string]any{"login": f.viewer}
	case strings.Contains(in.Query, "addPullRequestReview(input"):
		var v struct{ CommitOID string }
		json.Unmarshal(in.Variables["in"], &v)
		if f.pending != nil {
			fail("User can only have one pending review per pull request")
			break
		}
		f.pending = &fakePending{id: "PRR_pending", commit: v.CommitOID}
		out["addPullRequestReview"] = map[string]any{"pullRequestReview": map[string]any{"id": f.pending.id}}
	case strings.Contains(in.Query, "deletePullRequestReview"):
		f.pending = nil
		out["deletePullRequestReview"] = map[string]any{"clientMutationId": nil}
	default:
		for _, m := range aliasRE.FindAllStringSubmatch(in.Query, -1) {
			alias, field, vname := m[1], m[2], m[3]
			raw := in.Variables[vname]
			switch field {
			case "addPullRequestReviewThread":
				var v struct {
					Path, Body, Side, StartSide, SubjectType string
					Line, StartLine                          int
				}
				json.Unmarshal(raw, &v)
				if f.pending == nil {
					fail("no pending review")
					continue
				}
				if v.SubjectType != "FILE" {
					hunks := parseHunks(f.git(f.origin, "diff", "-M", "-U3", f.git(f.origin, "merge-base", f.pending.commit, "refs/heads/"+f.base), f.pending.commit))
					if !inHunk(hunks[v.Path], v.Side, v.StartLine, v.Line) {
						fail("Line could not be resolved: %s:%d %s", v.Path, v.Line, v.Side)
						out[alias] = nil
						continue
					}
				}
				c := GHReviewComment{
					Path: v.Path, Body: v.Body, Line: v.Line, OriginalLine: v.Line,
					StartLine: v.StartLine, OriginalStartLine: v.StartLine,
					Side: v.Side, CommitID: f.pending.commit, OriginalCommitID: f.pending.commit,
					User: GHUser{Login: f.viewer}, SubjectType: strings.ToLower(v.SubjectType),
				}
				if c.SubjectType == "" {
					c.SubjectType = "line"
				}
				f.pending.adds = append(f.pending.adds, c)
				id := fmt.Sprintf("PRRT_new%d", len(f.pending.adds))
				out[alias] = map[string]any{"thread": map[string]any{"id": id}}
			case "addPullRequestReviewThreadReply":
				var v struct{ PullRequestReviewThreadID, Body string }
				json.Unmarshal(raw, &v)
				t := f.threadByNode(v.PullRequestReviewThreadID)
				if t == nil || f.pending == nil {
					fail("no such thread %s", v.PullRequestReviewThreadID)
					continue
				}
				var root GHReviewComment
				for _, c := range f.comments {
					if c.ID == t.root {
						root = c
					}
				}
				root.InReplyTo, root.Body, root.User = t.root, v.Body, GHUser{Login: f.viewer}
				f.pending.adds = append(f.pending.adds, root)
				out[alias] = map[string]any{"comment": map[string]any{"id": "C"}}
			case "submitPullRequestReview":
				var v struct{ Event, Body string }
				json.Unmarshal(raw, &v)
				if f.pending == nil {
					fail("no pending review")
					continue
				}
				f.nextID++
				rv := GHReview{ID: f.nextID, User: GHUser{Login: f.viewer}, Body: v.Body, BodyHTML: "<p>" + v.Body + "</p>",
					State:    map[string]string{"COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}[v.Event],
					CommitID: f.pending.commit, SubmittedAt: time.Now()}
				f.reviews = append(f.reviews, rv)
				newThreads := 0
				for _, c := range f.pending.adds {
					c.ReviewID = rv.ID
					c = f.addCommentLocked(c)
					if c.InReplyTo == 0 {
						newThreads++
						// Thread node IDs made while pending become real.
						f.threads[c.ID].nodeID = fmt.Sprintf("PRRT_new%d", newThreads)
					}
				}
				f.pending = nil
				out[alias] = map[string]any{"pullRequestReview": map[string]any{"url": "https://github.com/review"}}
			case "resolveReviewThread", "unresolveReviewThread":
				var v struct{ ThreadID string }
				json.Unmarshal(raw, &v)
				t := f.threadByNode(v.ThreadID)
				if t == nil {
					fail("no thread %s", v.ThreadID)
					continue
				}
				t.resolved = field == "resolveReviewThread"
				out[alias] = map[string]any{"thread": map[string]any{"id": v.ThreadID, "isResolved": t.resolved}}
			default:
				fail("fake GitHub does not know %s", field)
			}
		}
		// The non-aliased forms the server sends one at a time.
		if strings.Contains(in.Query, "resolveReviewThread(input: $in)") || strings.Contains(in.Query, "unresolveReviewThread(input: $in)") {
			var v struct{ ThreadID string }
			json.Unmarshal(in.Variables["in"], &v)
			if t := f.threadByNode(v.ThreadID); t != nil {
				t.resolved = !strings.Contains(in.Query, "unresolveReviewThread")
			} else {
				fail("no thread %s", v.ThreadID)
			}
		}
	}
	resp := map[string]any{"data": out}
	if len(errs) > 0 {
		resp["errors"] = errs
	}
	json.NewEncoder(w).Encode(resp)
}
