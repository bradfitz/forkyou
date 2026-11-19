// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A server is the web interface.
type server struct {
	gh      *GitHub
	store   *Store
	gitDir  string // where the bare clones live
	gitBase string // base URL for cloning, such as "https://github.com"
	viewer  string // the logged-in GitHub user, or ""

	mu    sync.Mutex
	repos map[string]*GitRepo
	prs   map[PRKey]*prEntry

	mux *http.ServeMux
}

// A prEntry caches the last load of a pull request. Its lock is held
// while loading, so that a burst of requests for a pull request costs
// one trip to GitHub rather than one each.
type prEntry struct {
	mu sync.Mutex
	pr *PR
}

// How stale a cached pull request may be before a page reloads it. The
// pull request's own page asks GitHub every time it is opened, which with
// ETags is cheap; moving from file to file within it does not, since that
// is when the reviewer is reading rather than waiting for news.
const (
	maxAgePRPage = 5 * time.Second
	maxAgeDiff   = 2 * time.Minute
)

func newServer(gh *GitHub, store *Store, gitDir, gitBase string) *server {
	s := &server{
		gh:      gh,
		store:   store,
		gitDir:  gitDir,
		gitBase: strings.TrimSuffix(gitBase, "/"),
		repos:   map[string]*GitRepo{},
		prs:     map[PRKey]*prEntry{},
		mux:     http.NewServeMux(),
	}
	static, _ := fs.Sub(staticFS, "static")
	// Static files live one level down under a name no GitHub user can
	// have, out of the way of /owner/repo.
	s.mux.Handle("GET /_/{file}", http.StripPrefix("/_/", http.FileServerFS(static)))
	s.mux.Handle("GET /favicon.ico", http.RedirectHandler("/_/favicon.svg", http.StatusMovedPermanently))
	s.mux.HandleFunc("GET /{$}", s.handle(s.home))
	s.mux.HandleFunc("GET /go", s.handle(s.goURL))
	// Anything else, such as a github.com URL pasted after the host name.
	s.mux.HandleFunc("GET /", s.handle(s.goURL))
	s.mux.HandleFunc("POST /prefs", s.handle(s.prefs))
	s.mux.HandleFunc("GET /{owner}/{repo}/{$}", s.handle(s.repoPage))
	s.mux.HandleFunc("GET /{owner}/{repo}/pulls", s.handle(s.repoPage))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/{$}", s.prHandle(s.filesPage))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}", s.prHandle(s.filesPage))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/files", s.prHandle(s.filesPage))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/diff", s.prHandle(s.diffPage))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/rangediff", s.prHandle(s.rangeDiffPage))
	s.mux.HandleFunc("POST /{owner}/{repo}/pull/{num}/publish", s.prHandle(s.publishHandler))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/x/form", s.prHandle(s.formFrag))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/x/fileform", s.prHandle(s.formFrag))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/x/replyform", s.prHandle(s.replyFormFrag))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/x/editform", s.prHandle(s.editFormFrag))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/x/thread", s.prHandle(s.threadFrag))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/x/expand", s.prHandle(s.expandFrag))
	s.mux.HandleFunc("GET /{owner}/{repo}/pull/{num}/x/inline", s.prHandle(s.inlineFrag))
	s.mux.HandleFunc("POST /{owner}/{repo}/pull/{num}/x/comment", s.prHandle(s.commentFrag))
	s.mux.HandleFunc("POST /{owner}/{repo}/pull/{num}/x/edit", s.prHandle(s.editFrag))
	s.mux.HandleFunc("POST /{owner}/{repo}/pull/{num}/x/delete", s.prHandle(s.deleteFrag))
	s.mux.HandleFunc("POST /{owner}/{repo}/pull/{num}/x/resolve", s.prHandle(s.resolveFrag))
	s.mux.HandleFunc("POST /{owner}/{repo}/pull/{num}/x/reviewed", s.prHandle(s.reviewedFrag))
	s.mux.HandleFunc("POST /{owner}/{repo}/pull/{num}/x/snapreviewed", s.prHandle(s.snapReviewedFrag))
	s.mux.HandleFunc("POST /{owner}/{repo}/pull/{num}/x/message", s.prHandle(s.messageFrag))
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.mux.ServeHTTP(w, req)
}

// gitRepo returns the local clone of a repository.
func (s *server) gitRepo(owner, repo string) *GitRepo {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := owner + "/" + repo
	r := s.repos[name]
	if r == nil {
		r = &GitRepo{
			Dir:    filepath.Join(s.gitDir, owner, repo+".git"),
			Remote: s.gitBase + "/" + owner + "/" + repo + ".git",
			Token:  s.gh.Token,
		}
		s.repos[name] = r
	}
	return r
}

// getPR returns the pull request, loading it from GitHub if the cached
// copy is older than maxAge.
func (s *server) getPR(ctx context.Context, k PRKey, maxAge time.Duration) (*PR, error) {
	s.mu.Lock()
	e := s.prs[k]
	if e == nil {
		e = &prEntry{}
		s.prs[k] = e
	}
	s.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pr != nil && time.Since(e.pr.FetchedAt) < maxAge {
		return e.pr, nil
	}
	pr, err := s.loadPR(ctx, k)
	if err != nil {
		if e.pr != nil {
			// Better a page a minute old than no page at all.
			log.Printf("%v: reload failed, using cached copy: %v", k, err)
			return e.pr, nil
		}
		return nil, err
	}
	e.pr = pr
	return pr, nil
}

// invalidate makes the next getPR reload from GitHub.
func (s *server) invalidate(k PRKey) {
	s.mu.Lock()
	e := s.prs[k]
	s.mu.Unlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.pr != nil {
		e.pr.FetchedAt = time.Time{}
	}
	e.mu.Unlock()
}

// ---- handler plumbing --------------------------------------------------------

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func notFound(format string, args ...any) error {
	return &httpError{http.StatusNotFound, fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

func (s *server) handle(f func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if err := f(w, req); err != nil {
			code := http.StatusInternalServerError
			var he *httpError
			var ae *APIError
			switch {
			case errors.As(err, &he):
				code = he.code
			case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
				code = http.StatusNotFound
			}
			log.Printf("%s %s: %v", req.Method, req.URL, err)
			if req.Header.Get("X-Fragment") != "" {
				http.Error(w, err.Error(), code)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(code)
			tmpl.ExecuteTemplate(w, "error.html", &errorPage{
				head:  head{Title: "Error", Page: "error", Viewer: s.viewer},
				Error: err.Error(),
			})
		}
	}
}

type errorPage struct {
	head
	Error string
}

// prHandle wraps a handler for a page or fragment of one pull request.
func (s *server) prHandle(f func(http.ResponseWriter, *http.Request, PRKey) error) http.HandlerFunc {
	return s.handle(func(w http.ResponseWriter, req *http.Request) error {
		n, err := strconv.Atoi(req.PathValue("num"))
		if err != nil {
			return notFound("no pull request %q", req.PathValue("num"))
		}
		return f(w, req, PRKey{req.PathValue("owner"), req.PathValue("repo"), n})
	})
}

// ---- page context ----------------------------------------------------------------

// A head is what every page's top bar needs.
type head struct {
	Title    string
	Page     string
	Viewer   string
	Key      PRKey
	HasKey   bool
	FileName string
}

// RepoURL is the repository's page.
func (h *head) RepoURL() string { return "/" + h.Key.Owner + "/" + h.Key.Repo }

// A prPage is the context shared by every page and fragment of a pull
// request: the pull request, the comparison being shown, and the
// reviewer's own state.
type prPage struct {
	head
	PR *PR
	*View
	BaseParam   string
	TargetParam string
	FileParam   string
	Threads     []*Thread // published and draft
	Drafts      *Drafts
	Marks       *Reviewed
	Reviewed    map[string]bool // files reviewed in the target snapshot
	Unified     bool
	Context     int
	TabSize     int
}

func (s *server) prPage(ctx context.Context, req *http.Request, k PRKey, page string, maxAge time.Duration) (*prPage, error) {
	if req.FormValue("refresh") != "" {
		maxAge = 0
	}
	pr, err := s.getPR(ctx, k, maxAge)
	if err != nil {
		return nil, err
	}
	d, err := s.store.Drafts(k)
	if err != nil {
		return nil, err
	}
	marks, err := s.store.Reviewed(k)
	if err != nil {
		return nil, err
	}
	threads := allThreads(pr, d)
	base, target := req.FormValue("base"), req.FormValue("s")
	v, err := newView(ctx, pr, base, target, marks, threads)
	if err != nil {
		return nil, err
	}
	prefs := s.store.Prefs()
	p := &prPage{
		head:        head{Page: page, Viewer: s.viewer, Key: k, HasKey: true},
		PR:          pr,
		View:        v,
		BaseParam:   base,
		TargetParam: target,
		FileParam:   req.FormValue("f"),
		Threads:     threads,
		Drafts:      d,
		Marks:       marks,
		Unified:     prefs.Unified,
		Context:     prefs.Context,
		TabSize:     prefs.TabSize,
	}
	if u := req.FormValue("unified"); u != "" {
		p.Unified = u == "1"
	}
	if c := req.FormValue("context"); c != "" {
		if n, err := strconv.Atoi(c); err == nil {
			p.Context = n
		}
	}
	p.Reviewed = s.fileReviewed(ctx, pr, marks, v.Target)
	return p, nil
}

// PP returns p, for templates given a page that embeds it.
func (p *prPage) PP() *prPage { return p }

// PRURL is the pull request's page.
func (p *prPage) PRURL() string { return p.Key.URL() }

// query returns the query string keeping the comparison being shown.
func (p *prPage) query(extra ...string) string {
	q := url.Values{}
	if p.BaseParam != "" {
		q.Set("base", p.BaseParam)
	}
	if p.TargetParam != "" {
		q.Set("s", p.TargetParam)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// Query is the query string keeping the comparison being shown, with
// extra key-value pairs added.
func (p *prPage) Query(kv ...string) string { return p.query(kv...) }

// FilesURL is the file list for the comparison being shown.
func (p *prPage) FilesURL() string { return p.PRURL() + p.query() }

// DiffURL is the diff of a file in the comparison being shown.
func (p *prPage) DiffURL(file string) string { return p.PRURL() + "/diff" + p.query("f", file) }

// InlineURL is the diff of a file as a fragment for the file list.
func (p *prPage) InlineURL(file string) string { return p.X("inline") + p.query("f", file) }

// RangeDiffURL is the range-diff for the comparison being shown.
func (p *prPage) RangeDiffURL() string {
	q := url.Values{"s": {strconv.Itoa(p.Target.N)}}
	if p.Base != nil {
		q.Set("base", strconv.Itoa(p.Base.N))
	}
	return p.PRURL() + "/rangediff?" + q.Encode()
}

// X is the URL of one of the pull request's fragment handlers.
func (p *prPage) X(op string) string { return p.PRURL() + "/x/" + op }

// SelfURL is the page being shown, for forms to return to.
func (p *prPage) SelfURL() string {
	if p.Page == "diff" {
		return p.DiffURL(p.FileParam)
	}
	return p.FilesURL()
}

// SnapURL is the file list (or the current file's diff) at snapshot n,
// against its parent.
func (p *prPage) SnapURL(n int) string {
	q := url.Values{"s": {strconv.Itoa(n)}}
	if p.Page == "diff" {
		q.Set("f", p.FileParam)
		return p.PRURL() + "/diff?" + q.Encode()
	}
	return p.PRURL() + "?" + q.Encode()
}

// CompareURL compares snapshot a with snapshot b.
func (p *prPage) CompareURL(a, b int) string {
	return p.PRURL() + "?" + url.Values{"base": {strconv.Itoa(a)}, "s": {strconv.Itoa(b)}}.Encode()
}

// DraftCount is the number of unpublished comments.
func (p *prPage) DraftCount() int { return p.Drafts.Count() }

// CanPublish reports whether there is a token to publish with.
func (p *prPage) CanPublish() bool { return p.Viewer != "" }

// A threadFrag is what the thread template needs.
type threadFrag struct {
	*Thread
	P    *prPage
	Link string // where the thread lives, for lists away from the diff
}

// TF wraps a thread for the thread template.
func (p *prPage) TF(t *Thread) *threadFrag { return &threadFrag{Thread: t, P: p} }

// CanResolve reports whether the thread's resolution can be changed here.
func (t *threadFrag) CanResolve() bool { return t.Draft != nil || (t.NodeID != "" && t.P.Viewer != "") }

// ThreadLink is where a thread lives: for a comment on the newest
// snapshot, that snapshot; for one on an older snapshot, a comparison
// of that snapshot with the newest, which is the view that answers
// "what became of the line this was about".
func (p *prPage) ThreadLink(t *Thread) string {
	q := url.Values{"f": {t.File}}
	latest := p.PR.Latest()
	switch {
	case t.SnapshotN == 0 || latest == nil:
	case t.Side == "old":
		q.Set("base", "parent")
		q.Set("s", strconv.Itoa(t.SnapshotN))
	case t.SnapshotN == latest.N:
		q.Set("base", "parent")
	default:
		q.Set("base", strconv.Itoa(t.SnapshotN))
		q.Set("s", strconv.Itoa(latest.N))
	}
	u := p.PRURL() + "/diff?" + q.Encode()
	if t.ID != "" {
		u += "#thread-" + t.ID
	}
	return u
}

// fileReviewed returns the files marked reviewed in snapshot t. A file
// marked in an earlier snapshot carries the mark forward to a later one
// in which neither it nor its parent version changed, so that a new
// snapshot touching one file does not ask for the whole lot to be read
// again. An explicit unmark in t wins over a carried mark.
func (s *server) fileReviewed(ctx context.Context, pr *PR, marks *Reviewed, t *Snapshot) map[string]bool {
	out := map[string]bool{}
	var paths []string
	for _, sn := range pr.Snapshots {
		if sn.N > t.N || sn.Missing {
			continue
		}
		for p, on := range marks.Files[sn.Head] {
			if on {
				paths = append(paths, p)
			}
		}
	}
	paths = compactStrings(paths)
	if len(paths) == 0 {
		return out
	}
	tHead, _ := pr.Git.BlobOIDs(ctx, t.Head, paths)
	tBase, _ := pr.Git.BlobOIDs(ctx, t.Base, paths)
	for _, sn := range pr.Snapshots {
		if sn.N > t.N || sn.Missing {
			continue
		}
		m := marks.Files[sn.Head]
		if len(m) == 0 {
			continue
		}
		var sHead, sBase map[string]string
		if sn != t {
			sHead, _ = pr.Git.BlobOIDs(ctx, sn.Head, paths)
			sBase, _ = pr.Git.BlobOIDs(ctx, sn.Base, paths)
		}
		for p, on := range m {
			if !on {
				continue
			}
			if sn == t || (sHead[p] == tHead[p] && sBase[p] == tBase[p]) {
				out[p] = true
			}
		}
	}
	for p, on := range marks.Files[t.Head] {
		if !on {
			delete(out, p)
		}
	}
	return out
}

// ---- home and repository pages -------------------------------------------------------

type prListItem struct {
	Key       PRKey
	Title     string
	Author    string
	Updated   time.Time
	Draft     bool
	Comments  int
	Drafts    int
	RepoLabel string
}

func (i *prListItem) URL() string { return i.Key.URL() }

type homePage struct {
	head
	Sections []*homeSection
	Recent   []RecentPR
	Error    string
}

type homeSection struct {
	Title string
	Items []*prListItem
	Error string
}

var repoURLRE = regexp.MustCompile(`/repos/([^/]+)/([^/]+)$`)

func (s *server) home(w http.ResponseWriter, req *http.Request) error {
	p := &homePage{head: head{Title: "forkyou", Page: "home", Viewer: s.viewer}}
	if s.viewer != "" {
		queries := []struct{ title, q string }{
			{"Review requested", "is:open is:pr review-requested:@me archived:false"},
			{"Your pull requests", "is:open is:pr author:@me archived:false"},
			{"Reviewed by you, still open", "is:open is:pr reviewed-by:@me -author:@me archived:false"},
		}
		p.Sections = make([]*homeSection, len(queries))
		var wg sync.WaitGroup
		for i, q := range queries {
			sec := &homeSection{Title: q.title}
			p.Sections[i] = sec
			wg.Go(func() {
				var res GHSearchResult
				err := s.gh.Get(req.Context(), "/search/issues?per_page=50&sort=updated&q="+url.QueryEscape(q.q), &res)
				if err != nil {
					sec.Error = err.Error()
					return
				}
				for _, it := range res.Items {
					m := repoURLRE.FindStringSubmatch(it.RepositoryURL)
					if m == nil {
						continue
					}
					sec.Items = append(sec.Items, &prListItem{
						Key: PRKey{m[1], m[2], it.Number}, Title: it.Title, Author: it.User.Login,
						Updated: it.UpdatedAt, Draft: it.Draft, Comments: it.Comments,
						RepoLabel: m[1] + "/" + m[2],
					})
				}
			})
		}
		wg.Wait()
	}
	p.Recent = s.store.Recent()
	if len(p.Recent) > 15 {
		p.Recent = p.Recent[:15]
	}
	return render(w, "home.html", p)
}

// goURL turns a github.com URL into the same page here.
func (s *server) goURL(w http.ResponseWriter, req *http.Request) error {
	u := req.FormValue("u")
	if req.URL.Path != "/go" {
		u = strings.TrimPrefix(req.URL.Path, "/")
	}
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "github.com/")
	f := strings.Split(strings.Trim(u, "/"), "/")
	switch {
	case len(f) >= 4 && (f[2] == "pull" || f[2] == "pulls") && isNum(f[3]):
		http.Redirect(w, req, PRKey{f[0], f[1], atoi(f[3])}.URL(), http.StatusFound)
	case len(f) == 2 && f[0] != "" && f[1] != "":
		http.Redirect(w, req, "/"+f[0]+"/"+f[1], http.StatusFound)
	default:
		return badRequest("not a pull request URL: %q (want owner/repo/pull/N)", u)
	}
	return nil
}

func isNum(s string) bool { _, err := strconv.Atoi(s); return err == nil }
func atoi(s string) int   { n, _ := strconv.Atoi(s); return n }

type repoPage struct {
	head
	Owner, Repo string
	Items       []*prListItem
}

func (s *server) repoPage(w http.ResponseWriter, req *http.Request) error {
	owner, repo := req.PathValue("owner"), req.PathValue("repo")
	pulls, err := GetAll[GHPull](req.Context(), s.gh, fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=100", owner, repo))
	if err != nil {
		return err
	}
	p := &repoPage{head: head{Title: owner + "/" + repo, Page: "repo", Viewer: s.viewer, Key: PRKey{Owner: owner, Repo: repo}}, Owner: owner, Repo: repo}
	for _, pl := range pulls {
		k := PRKey{owner, repo, pl.Number}
		it := &prListItem{Key: k, Title: pl.Title, Author: pl.User.Login, Updated: pl.UpdatedAt, Draft: pl.Draft}
		if d, err := s.store.Drafts(k); err == nil {
			it.Drafts = d.Count()
		}
		p.Items = append(p.Items, it)
	}
	return render(w, "repo.html", p)
}

// render executes a template into a buffer and sends it only if that
// succeeded, so that a failure partway is an error page and not half a page.
func render(w http.ResponseWriter, name string, data any) error {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := w.Write(buf.Bytes())
	return err
}

// ---- the pull request page ---------------------------------------------------------

type fileRow struct {
	*ViewFile
	URL        string
	InlineURL  string
	Reviewed   bool
	Resolved   int
	Unresolved int
	DraftN     int
}

// Threads is the number of comment threads on the file.
func (f *fileRow) Threads() int { return f.Resolved + f.Unresolved }

// Hidden reports whether the row is left out of the list until asked
// for: a file only a rebase touched, with nobody having said anything
// about it.
func (f *fileRow) Hidden() bool { return f.RebaseOnly && !f.Own && f.Threads() == 0 }

// Name is the path, shortened for display.
func (f *fileRow) Name() string { return f.Path }

type snapRow struct {
	*Snapshot
	Sel      bool
	IsBase   bool
	Reviewed bool
	Rebased  bool // sits on a different base than the snapshot before it
	Reviews  []*GHReview
}

type filesPage struct {
	*prPage
	Files           []*fileRow
	RebaseOnlyCount int
	Snaps           []*snapRow
	Commits         []*Commit
	Conversation    []*ConvItem
	Unresolved      []*threadFrag
	Resolved        []*threadFrag
	Approvals       []string
	ChangesReq      []string
	Flash           string
}

// fileRows builds the file list rows, with their comment counts.
func (p *prPage) fileRows() []*fileRow {
	var rows []*fileRow
	for _, f := range p.Files {
		r := &fileRow{ViewFile: f, URL: p.DiffURL(f.Path), InlineURL: p.InlineURL(f.Path), Reviewed: p.Reviewed[f.Path]}
		for _, t := range p.Threads {
			if t.File != f.Path {
				continue
			}
			if t.Resolved {
				r.Resolved++
			} else {
				r.Unresolved++
			}
			if t.DraftComment() != nil {
				r.DraftN++
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func (s *server) filesPage(w http.ResponseWriter, req *http.Request, k PRKey) error {
	if strings.HasSuffix(req.URL.Path, "/files") {
		http.Redirect(w, req, k.URL(), http.StatusFound)
		return nil
	}
	ctx := req.Context()
	pp, err := s.prPage(ctx, req, k, "files", maxAgePRPage)
	if err != nil {
		return err
	}
	pr := pp.PR
	pp.Title = fmt.Sprintf("%s · #%d", pr.Pull.Title, k.Number)
	p := &filesPage{prPage: pp, Flash: req.FormValue("flash")}
	p.Files = pp.fileRows()
	for _, f := range p.Files {
		if f.Hidden() {
			p.RebaseOnlyCount++
		}
	}
	reviewsAt := map[string][]*GHReview{}
	latestState := map[string]string{}
	for i := range pr.Reviews {
		r := &pr.Reviews[i]
		if r.State == "PENDING" {
			continue
		}
		reviewsAt[r.CommitID] = append(reviewsAt[r.CommitID], r)
		if r.State != "COMMENTED" {
			latestState[r.User.Login] = r.State
		}
	}
	for who, st := range latestState {
		switch st {
		case "APPROVED":
			p.Approvals = append(p.Approvals, who)
		case "CHANGES_REQUESTED":
			p.ChangesReq = append(p.ChangesReq, who)
		}
	}
	slices.Sort(p.Approvals)
	slices.Sort(p.ChangesReq)
	var prev *Snapshot
	for _, sn := range pr.Snapshots {
		row := &snapRow{
			Snapshot: sn,
			Sel:      sn == pp.Target,
			IsBase:   sn == pp.Base,
			Reviewed: pp.Marks.Snapshots[sn.Head],
			Reviews:  reviewsAt[sn.Head],
		}
		if prev != nil && !sn.Missing && !prev.Missing && prev.Base != sn.Base {
			row.Rebased = true
		}
		if !sn.Missing {
			prev = sn
		}
		p.Snaps = append(p.Snaps, row)
	}
	if cs, err := pr.Git.Log(ctx, pp.Target.Base, pp.Target.Head); err == nil {
		p.Commits = cs
	}
	p.Conversation = pr.Conversation()
	for _, t := range pp.Threads {
		tf := &threadFrag{Thread: t, P: pp, Link: pp.ThreadLink(t)}
		if t.Resolved {
			p.Resolved = append(p.Resolved, tf)
		} else {
			p.Unresolved = append(p.Unresolved, tf)
		}
	}
	byTime := func(a, b *threadFrag) int { return b.Latest().Compare(a.Latest()) }
	slices.SortFunc(p.Unresolved, byTime)
	slices.SortFunc(p.Resolved, byTime)

	s.store.NoteRecent(RecentPR{Key: k, Title: pr.Pull.Title, Author: pr.Pull.User.Login, State: pr.Pull.State, Viewed: time.Now()})
	return render(w, "files.html", p)
}

// ---- the diff page -----------------------------------------------------------------

type diffPage struct {
	*prPage
	File           *fileRow
	Rows           []*renderRow
	Top            []*threadFrag // comments on the whole file, and stale ones
	Binary         bool
	Rebased        bool // some row's edit came along with a rebase
	FileRebaseOnly bool
	Prev, Next     *fileRow
	FilesJSON      template.JS
}

// ShowRebaseNote reports whether the diff needs the note explaining its
// muted colors.
func (p *diffPage) ShowRebaseNote() bool { return p.Rebased }

// Cols is the number of columns in the diff table.
func (p *prPage) Cols() int {
	if p.Unified {
		return 2
	}
	return 4
}

// FormURL is the URL that opens a comment form on a line. side is the
// side of the diff, "left" or "right".
func (p *prPage) FormURL(side string, line int) string {
	return p.X("form") + p.query("f", p.FileParam, "side", side, "line", strconv.Itoa(line), "unified", boolParam(p.Unified))
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// hasThread reports which rows carry a comment, so that collapsing leaves
// them showing.
func hasThread(leftAt, rightAt map[int][]*Thread) func(Row) bool {
	if len(leftAt) == 0 && len(rightAt) == 0 {
		return nil
	}
	return func(r Row) bool {
		return r.L.Num > 0 && len(leftAt[r.L.Num]) > 0 ||
			r.R.Num > 0 && len(rightAt[r.R.Num]) > 0
	}
}

// renderRows turns diff rows into rendered rows, attaching comment threads.
func (p *prPage) renderRows(rows []Row, leftAt, rightAt map[int][]*Thread) []*renderRow {
	var out []*renderRow
	for _, row := range rows {
		rr := &renderRow{Row: row}
		rr.LeftHTML = lineHTML(row.L.Text, row.L.Spans)
		rr.RightHTML = lineHTML(row.R.Text, row.R.Spans)
		if row.L.Num > 0 {
			rr.LeftThreads = leftAt[row.L.Num]
		}
		if row.R.Num > 0 {
			rr.RightThreads = rightAt[row.R.Num]
		}
		if row.Kind == RowSkip {
			rr.ExpandURL = p.X("expand") + p.query("f", p.FileParam,
				"from", strconv.Itoa(row.LFrom), "to", strconv.Itoa(row.LFrom+row.Count-1),
				"rfrom", strconv.Itoa(row.RFrom), "unified", boolParam(p.Unified))
		}
		out = append(out, rr)
	}
	return out
}

// fileThreads places the threads of one file in the view.
func (p *prPage) fileThreads(ctx context.Context, f *ViewFile) (old, new []byte, leftAt, rightAt map[int][]*Thread) {
	old, new = p.Contents(ctx, f)
	oldLines, _ := splitLines(old)
	newLines, _ := splitLines(new)
	left, right := p.PlaceThreads(p.Threads, f.Path, oldLines, newLines)
	return old, new, ThreadsByLine(left), ThreadsByLine(right)
}

func (s *server) diffPage(w http.ResponseWriter, req *http.Request, k PRKey) error {
	ctx := req.Context()
	pp, err := s.prPage(ctx, req, k, "diff", maxAgeDiff)
	if err != nil {
		return err
	}
	f := pp.File(pp.FileParam)
	if f == nil {
		// A file with no diff in this comparison can still be read.
		if pp.PR.blob(ctx, pp.RightRev(), pp.FileParam) == nil && pp.PR.blob(ctx, pp.LeftRev(), pp.FileParam) == nil {
			return notFound("no file %q in snapshot %d", pp.FileParam, pp.Target.N)
		}
		f = &ViewFile{Path: pp.FileParam}
	}
	pp.Title = path.Base(f.Path) + " · #" + strconv.Itoa(k.Number) + " " + pp.PR.Pull.Title
	pp.FileName = f.Path
	_, _, leftAt, rightAt := pp.fileThreads(ctx, f)
	fd := pp.Diff(ctx, f)
	rows := Collapse(fd.Rows, pp.Context, hasThread(leftAt, rightAt))
	if pp.Unified {
		rows = Unified(rows)
	}
	p := &diffPage{
		prPage:         pp,
		Binary:         fd.Binary,
		Rebased:        AnyRebased(fd.Rows),
		FileRebaseOnly: AllRebased(fd.Rows),
	}
	for _, t := range append(slices.Clone(leftAt[0]), rightAt[0]...) {
		p.Top = append(p.Top, pp.TF(t))
	}
	p.Rows = pp.renderRows(rows, leftAt, rightAt)

	type fileEntry struct {
		Path     string `json:"path"`
		URL      string `json:"url"`
		Threads  int    `json:"comments"`
		Reviewed bool   `json:"reviewed"`
		Rebase   bool   `json:"rebase"`
	}
	var entries []fileEntry
	frs := pp.fileRows()
	for i, fr := range frs {
		entries = append(entries, fileEntry{fr.Path, fr.URL, fr.Threads(), fr.Reviewed, fr.Hidden()})
		if fr.Path != f.Path {
			continue
		}
		p.File = fr
		if i > 0 {
			p.Prev = frs[i-1]
		}
		if i+1 < len(frs) {
			p.Next = frs[i+1]
		}
	}
	if p.File == nil {
		p.File = &fileRow{ViewFile: f, Reviewed: pp.Reviewed[f.Path]}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	p.FilesJSON = template.JS(data)
	return render(w, "diff.html", p)
}

// ---- range-diff ------------------------------------------------------------------------

type rangeDiffPage struct {
	*prPage
	HTML  template.HTML
	Error string
}

func (s *server) rangeDiffPage(w http.ResponseWriter, req *http.Request, k PRKey) error {
	ctx := req.Context()
	pp, err := s.prPage(ctx, req, k, "rangediff", maxAgeDiff)
	if err != nil {
		return err
	}
	pp.Title = fmt.Sprintf("range-diff · #%d %s", k.Number, pp.PR.Pull.Title)
	if pp.Base == nil {
		// Range-diff compares two snapshots, so with nothing chosen the
		// natural pair is this one and the one before it.
		for i := pp.Target.N - 1; i >= 1; i-- {
			if sn := pp.PR.Snapshot(i); !sn.Missing {
				pp.Base = sn
				break
			}
		}
	}
	p := &rangeDiffPage{prPage: pp}
	if pp.Base == nil {
		p.Error = "There is only one snapshot, so there is nothing to compare it with."
		return render(w, "rangediff.html", p)
	}
	out, err := pp.PR.Git.RangeDiff(ctx, pp.Base.Base, pp.Base.Head, pp.Target.Base, pp.Target.Head)
	if err != nil {
		p.Error = err.Error()
	} else {
		p.HTML = rangeDiffHTML(out)
	}
	return render(w, "rangediff.html", p)
}

// ---- fragments -------------------------------------------------------------------------

// fragPage is the page context for a fragment.
func (s *server) fragPage(req *http.Request, k PRKey) (*prPage, error) {
	return s.prPage(req.Context(), req, k, "diff", maxAgeDiff)
}

// expandFrag returns the rows hidden behind an "N unchanged lines" marker.
func (s *server) expandFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	ctx := req.Context()
	p, err := s.fragPage(req, k)
	if err != nil {
		return err
	}
	f := p.File(p.FileParam)
	if f == nil {
		f = &ViewFile{Path: p.FileParam}
	}
	old, new, leftAt, rightAt := p.fileThreads(ctx, f)
	oldLines, _ := splitLines(old)
	newLines, _ := splitLines(new)
	from, to, rfrom := atoi(req.FormValue("from")), atoi(req.FormValue("to")), atoi(req.FormValue("rfrom"))
	var rows []Row
	for i := 0; from+i <= to; i++ {
		l, r := from+i, rfrom+i
		if l < 1 || l > len(oldLines) || r < 1 || r > len(newLines) {
			break
		}
		rows = append(rows, Row{
			Kind: RowEqual,
			L:    Line{Num: l, Text: oldLines[l-1]},
			R:    Line{Num: r, Text: newLines[r-1]},
		})
	}
	return render(w, "rows", &diffPage{prPage: p, Rows: p.renderRows(rows, leftAt, rightAt)})
}

// inlineFrag renders a file's diff as a table, for the file list.
func (s *server) inlineFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	ctx := req.Context()
	p, err := s.fragPage(req, k)
	if err != nil {
		return err
	}
	f := p.File(p.FileParam)
	if f == nil {
		return notFound("no file %q in this comparison", p.FileParam)
	}
	_, _, leftAt, rightAt := p.fileThreads(ctx, f)
	fd := p.Diff(ctx, f)
	rows := Collapse(fd.Rows, p.Context, hasThread(leftAt, rightAt))
	if p.Unified {
		rows = Unified(rows)
	}
	dp := &diffPage{prPage: p, Binary: fd.Binary, Rows: p.renderRows(rows, leftAt, rightAt)}
	return render(w, "inline", dp)
}

// formArg is what a new-comment form needs.
type formArg struct {
	P     *prPage
	Side  string // "left" or "right"
	Line  int
	Start int
}

// Lines describes the lines being commented on.
func (f *formArg) Lines() string {
	switch {
	case f.Line == 0:
		return "this file"
	case f.Start > 0 && f.Start != f.Line:
		return fmt.Sprintf("lines %d-%d", f.Start, f.Line)
	}
	return fmt.Sprintf("line %d", f.Line)
}

// formFrag returns a form for a new comment, in a table row to go below
// the line it is about, or for a file comment, on its own.
func (s *server) formFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	p, err := s.fragPage(req, k)
	if err != nil {
		return err
	}
	a := &formArg{P: p, Side: req.FormValue("side"), Line: atoi(req.FormValue("line")), Start: atoi(req.FormValue("start"))}
	if a.Side != "left" {
		a.Side = "right"
	}
	if a.Start > a.Line {
		a.Start, a.Line = a.Line, a.Start
	}
	if strings.HasSuffix(req.URL.Path, "/fileform") || a.Line == 0 {
		a.Line, a.Start = 0, 0
		return render(w, "newform", a)
	}
	return render(w, "commentrow", a)
}

// sideTarget says what a side of the diff being shown is: which snapshot,
// and which side of that snapshot.
func (p *prPage) sideTarget(side string) (head, snapSide, rev string) {
	if side == "left" {
		if p.Base != nil {
			return p.Base.Head, "new", p.Base.Head
		}
		return p.Target.Head, "old", p.Target.Base
	}
	return p.Target.Head, "new", p.Target.Head
}

// commentFrag saves a new draft: a new thread, or a reply to a thread.
func (s *server) commentFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	ctx := req.Context()
	body := strings.TrimRight(req.FormValue("body"), " \t\r\n")
	if strings.TrimSpace(body) == "" {
		return badRequest("empty comment")
	}
	p, err := s.fragPage(req, k)
	if err != nil {
		return err
	}
	if id := req.FormValue("thread"); id != "" {
		t := p.PR.Thread(id)
		if t == nil {
			return notFound("no thread %s", id)
		}
		err := s.store.UpdateDrafts(k, func(d *Drafts) error {
			for _, r := range d.Replies {
				if r.ThreadID == id {
					r.Body, r.Resolve = body, req.FormValue("resolve") == "1"
					return nil
				}
			}
			d.Replies = append(d.Replies, &DraftReply{
				ID: d.newID(), ThreadID: id, Body: body, Resolve: req.FormValue("resolve") == "1", Created: time.Now(),
			})
			return nil
		})
		if err != nil {
			return err
		}
		return s.renderThread(w, req, k, id)
	}

	side := req.FormValue("side")
	if side != "left" {
		side = "right"
	}
	head, snapSide, rev := p.sideTarget(side)
	file := req.FormValue("f")
	line, start := atoi(req.FormValue("line")), atoi(req.FormValue("start"))
	if start > line {
		start, line = line, start
	}
	if start == line {
		start = 0
	}
	anchor := ""
	if line > 0 {
		lines := p.PR.lines(ctx, rev, file)
		if line > len(lines) {
			return badRequest("%s has no line %d", file, line)
		}
		anchor = lines[line-1]
	}
	var dt *DraftThread
	err = s.store.UpdateDrafts(k, func(d *Drafts) error {
		dt = &DraftThread{
			ID: d.newID(), Head: head, File: file, Side: snapSide, Line: line, StartLine: start,
			Anchor: anchor, Body: body, Resolved: req.FormValue("resolve") == "1", Created: time.Now(),
		}
		d.Threads = append(d.Threads, dt)
		return nil
	})
	if err != nil {
		return err
	}
	// Draw it the way the page will once reloaded.
	d, _ := s.store.Drafts(k)
	p.Drafts = d
	p.Threads = allThreads(p.PR, d)
	var t *Thread
	for _, x := range p.Threads {
		if x.ID == dt.ID {
			t = x.clone()
		}
	}
	t.ShowLine = line
	w.Header().Set("X-Drafts", strconv.Itoa(d.Count()))
	if line == 0 {
		return render(w, "thread", p.TF(t))
	}
	return render(w, "threadrow", map[string]any{"P": p, "Side": side, "T": p.TF(t)})
}

// renderThread draws one thread as it now stands.
func (s *server) renderThread(w http.ResponseWriter, req *http.Request, k PRKey, id string) error {
	p, err := s.fragPage(req, k)
	if err != nil {
		return err
	}
	w.Header().Set("X-Drafts", strconv.Itoa(p.Drafts.Count()))
	for _, t := range p.Threads {
		if t.ID == id {
			tf := p.TF(t)
			if req.FormValue("link") != "" {
				tf.Link = p.ThreadLink(t)
			}
			return render(w, "thread", tf)
		}
	}
	// Gone: a deleted draft thread. An empty answer removes it.
	return nil
}

func (s *server) threadFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	return s.renderThread(w, req, k, req.FormValue("thread"))
}

func (s *server) replyFormFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	p, err := s.fragPage(req, k)
	if err != nil {
		return err
	}
	id := req.FormValue("thread")
	t := p.PR.Thread(id)
	if t == nil {
		return notFound("no thread %s", id)
	}
	return render(w, "replyform", map[string]any{"P": p, "T": t, "Link": req.FormValue("link")})
}

// findDraft returns the draft comment with the given ID.
func findDraft(d *Drafts, id string) (dt *DraftThread, dr *DraftReply) {
	for _, t := range d.Threads {
		if t.ID == id {
			return t, nil
		}
	}
	for _, r := range d.Replies {
		if r.ID == id {
			return nil, r
		}
	}
	return nil, nil
}

func (s *server) editFormFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	p, err := s.fragPage(req, k)
	if err != nil {
		return err
	}
	id := req.FormValue("comment")
	dt, dr := findDraft(p.Drafts, id)
	var body, thread string
	switch {
	case dt != nil:
		body, thread = dt.Body, dt.ID
	case dr != nil:
		body, thread = dr.Body, dr.ThreadID
	default:
		return notFound("no draft %s", id)
	}
	return render(w, "editform", map[string]any{"P": p, "ID": id, "Body": body, "Thread": thread, "Link": req.FormValue("link")})
}

func (s *server) editFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	id := req.FormValue("comment")
	body := strings.TrimRight(req.FormValue("body"), " \t\r\n")
	var thread string
	err := s.store.UpdateDrafts(k, func(d *Drafts) error {
		dt, dr := findDraft(d, id)
		switch {
		case dt != nil:
			dt.Body, thread = body, dt.ID
		case dr != nil:
			dr.Body, thread = body, dr.ThreadID
		default:
			return notFound("no draft %s", id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.renderThread(w, req, k, thread)
}

func (s *server) deleteFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	id := req.FormValue("comment")
	var thread string
	err := s.store.UpdateDrafts(k, func(d *Drafts) error {
		for i, t := range d.Threads {
			if t.ID == id {
				d.Threads = slices.Delete(d.Threads, i, i+1)
				thread = t.ID
				return nil
			}
		}
		for i, r := range d.Replies {
			if r.ID == id {
				d.Replies = slices.Delete(d.Replies, i, i+1)
				thread = r.ThreadID
				return nil
			}
		}
		return notFound("no draft %s", id)
	})
	if err != nil {
		return err
	}
	return s.renderThread(w, req, k, thread)
}

// resolveFrag flips a thread between resolved and unresolved. A draft
// thread just changes its draft. A published thread changes on GitHub at
// once, as GitHub's own button does: resolving is not a remark to be
// composed with the others, and waiting until publish to do it would
// leave the thread looking open to everyone else in the meantime.
func (s *server) resolveFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	ctx := req.Context()
	id := req.FormValue("thread")
	if strings.HasPrefix(id, "d") {
		err := s.store.UpdateDrafts(k, func(d *Drafts) error {
			dt, _ := findDraft(d, id)
			if dt == nil {
				return notFound("no draft %s", id)
			}
			dt.Resolved = !dt.Resolved
			return nil
		})
		if err != nil {
			return err
		}
		return s.renderThread(w, req, k, id)
	}
	pr, err := s.getPR(ctx, k, maxAgeDiff)
	if err != nil {
		return err
	}
	t := pr.Thread(id)
	if t == nil || t.NodeID == "" {
		return badRequest("thread %s cannot be resolved from here", id)
	}
	if err := s.setResolved(ctx, t.NodeID, !t.Resolved); err != nil {
		return err
	}
	s.invalidate(k)
	return s.renderThread(w, req, k, id)
}

// reviewedFrag toggles a file's reviewed mark in the target snapshot.
func (s *server) reviewedFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	p, err := s.fragPage(req, k)
	if err != nil {
		return err
	}
	file := req.FormValue("f")
	on := !p.Reviewed[file]
	err = s.store.UpdateReviewed(k, func(r *Reviewed) {
		m := r.Files[p.Target.Head]
		if m == nil {
			m = map[string]bool{}
			r.Files[p.Target.Head] = m
		}
		m[file] = on
	})
	if err != nil {
		return err
	}
	rebaseOnly := false
	if f := p.File(file); f != nil {
		rebaseOnly = f.RebaseOnly
	}
	return render(w, "reviewedbutton", map[string]any{
		"URL": p.X("reviewed") + p.Query("f", file), "Reviewed": on, "RebaseOnly": rebaseOnly,
	})
}

// snapReviewedFrag toggles a snapshot's reviewed mark, and with it the
// mark on every file the snapshot changes: saying a snapshot has been
// read is saying its files have.
func (s *server) snapReviewedFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	ctx := req.Context()
	pr, err := s.getPR(ctx, k, maxAgeDiff)
	if err != nil {
		return err
	}
	sn := pr.Snapshot(atoi(req.FormValue("n")))
	if sn == nil || sn.Missing {
		return notFound("no snapshot %s", req.FormValue("n"))
	}
	files, err := pr.Git.DiffTrees(ctx, sn.Base, sn.Head)
	if err != nil {
		return err
	}
	marks, err := s.store.Reviewed(k)
	if err != nil {
		return err
	}
	on := !marks.Snapshots[sn.Head]
	err = s.store.UpdateReviewed(k, func(r *Reviewed) {
		r.Snapshots[sn.Head] = on
		m := map[string]bool{}
		for _, f := range files {
			m[f.Path] = on
		}
		r.Files[sn.Head] = m
	})
	if err != nil {
		return err
	}
	// The reviewed buttons all over the page have just changed.
	w.Header().Set("X-Refresh", "1")
	return render(w, "snapreviewedbutton", map[string]any{"URL": req.URL.String(), "Reviewed": on})
}

// messageFrag saves the draft of the review's own message as it is typed,
// so that it survives a reload.
func (s *server) messageFrag(w http.ResponseWriter, req *http.Request, k PRKey) error {
	msg := req.FormValue("message")
	if err := s.store.UpdateDrafts(k, func(d *Drafts) error { d.Message = msg; return nil }); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// publishHandler publishes the drafts as a review.
func (s *server) publishHandler(w http.ResponseWriter, req *http.Request, k PRKey) error {
	ctx := req.Context()
	msg := req.FormValue("message")
	event := req.FormValue("event")
	switch event {
	case "COMMENT", "APPROVE", "REQUEST_CHANGES":
	default:
		event = "COMMENT"
	}
	// Keep the message whatever happens next, so that a failure costs
	// nothing but a retry.
	if err := s.store.UpdateDrafts(k, func(d *Drafts) error { d.Message = msg; return nil }); err != nil {
		return err
	}
	// Publishing is against the head as it is right now.
	pr, err := s.getPR(ctx, k, 0)
	if err != nil {
		return err
	}
	d, err := s.store.Drafts(k)
	if err != nil {
		return err
	}
	res, err := s.publish(ctx, pr, d, event, msg)
	if err != nil {
		ret := k.URL() + "?" + url.Values{"flash": {"Publishing failed: " + err.Error()}}.Encode()
		http.Redirect(w, req, ret, http.StatusSeeOther)
		return nil
	}
	done := map[string]bool{}
	for _, id := range res.Published {
		done[id] = true
	}
	err = s.store.UpdateDrafts(k, func(d *Drafts) error {
		d.Threads = slices.DeleteFunc(d.Threads, func(t *DraftThread) bool { return done[t.ID] })
		d.Replies = slices.DeleteFunc(d.Replies, func(r *DraftReply) bool { return done[r.ID] })
		d.Message = ""
		return nil
	})
	if err != nil {
		return err
	}
	s.invalidate(k)
	ret := req.FormValue("return")
	if ret == "" || !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") {
		ret = k.URL()
	}
	http.Redirect(w, req, ret, http.StatusSeeOther)
	return nil
}

// prefs saves the diff preferences.
func (s *server) prefs(w http.ResponseWriter, req *http.Request) error {
	p := s.store.Prefs()
	if n, err := strconv.Atoi(req.FormValue("context")); err == nil && n >= 0 {
		p.Context = n
	}
	if n, err := strconv.Atoi(req.FormValue("tabsize")); err == nil && n >= 1 && n <= 16 {
		p.TabSize = n
	}
	p.Unified = req.FormValue("unified") == "1"
	if err := s.store.SetPrefs(p); err != nil {
		return err
	}
	ret := req.FormValue("return")
	if ret == "" || !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") {
		ret = "/"
	}
	http.Redirect(w, req, ret, http.StatusSeeOther)
	return nil
}
