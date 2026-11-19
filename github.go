// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A GitHub is a client for the GitHub REST and GraphQL APIs.
//
// GET requests are cached on disk with their ETags, so that asking again
// for something that has not changed costs a 304, which GitHub does not
// count against the rate limit.
type GitHub struct {
	API      string // REST API base URL, such as "https://api.github.com"
	GraphQL  string // GraphQL endpoint URL
	Token    string // may be empty for anonymous, read-only use
	CacheDir string // directory for the ETag cache; empty disables it
	HTTP     *http.Client

	mu  sync.Mutex
	mem map[string]*cacheEntry
}

type cacheEntry struct {
	ETag string          `json:"etag"`
	Link string          `json:"link,omitempty"`
	Body json.RawMessage `json:"body"`
}

// tokenFromGH returns an API token for GitHub, from $GITHUB_TOKEN or else
// from the gh command's stored credentials.
func tokenFromGH() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	t := strings.TrimSpace(string(out))
	if strings.ContainsAny(t, " \n") {
		// Not a token: some other gh printing usage text.
		return ""
	}
	return t
}

// An APIError is a non-2xx response from GitHub.
type APIError struct {
	Status  int
	Method  string
	URL     string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.URL, e.Status, e.Message)
}

func (g *GitHub) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return http.DefaultClient
}

func (g *GitHub) newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	// The "full" media type returns body_html alongside the raw body, so
	// comments are shown exactly as GitHub renders them without this
	// program having to implement GitHub-flavored markdown.
	req.Header.Set("Accept", "application/vnd.github.full+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (g *GitHub) cachePath(url string) string {
	h := sha256.Sum256([]byte(g.Token + "\x00" + url))
	return filepath.Join(g.CacheDir, hex.EncodeToString(h[:2]), hex.EncodeToString(h[:]))
}

func (g *GitHub) loadCache(url string) *cacheEntry {
	g.mu.Lock()
	e := g.mem[url]
	g.mu.Unlock()
	if e != nil || g.CacheDir == "" {
		return e
	}
	data, err := os.ReadFile(g.cachePath(url))
	if err != nil {
		return nil
	}
	e = new(cacheEntry)
	if json.Unmarshal(data, e) != nil {
		return nil
	}
	g.mu.Lock()
	if g.mem == nil {
		g.mem = map[string]*cacheEntry{}
	}
	g.mem[url] = e
	g.mu.Unlock()
	return e
}

func (g *GitHub) storeCache(url string, e *cacheEntry) {
	g.mu.Lock()
	if g.mem == nil {
		g.mem = map[string]*cacheEntry{}
	}
	g.mem[url] = e
	g.mu.Unlock()
	if g.CacheDir == "" {
		return
	}
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	writeFileAtomic(g.cachePath(url), data)
}

// get fetches one page of a REST resource, returning its body and the
// URL of the next page, if any.
func (g *GitHub) get(ctx context.Context, path string) (body []byte, next string, err error) {
	url := path
	if !strings.HasPrefix(url, "http") {
		url = g.API + path
	}
	req, err := g.newRequest(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	cached := g.loadCache(url)
	if cached != nil && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && cached != nil {
		return cached.Body, nextLink(cached.Link), nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", apiError(resp, data)
	}
	if et := resp.Header.Get("ETag"); et != "" {
		g.storeCache(url, &cacheEntry{ETag: et, Link: resp.Header.Get("Link"), Body: data})
	}
	return data, nextLink(resp.Header.Get("Link")), nil
}

func apiError(resp *http.Response, data []byte) error {
	var m struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	json.Unmarshal(data, &m)
	msg := m.Message
	for _, e := range m.Errors {
		msg += "; " + e.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	return &APIError{Status: resp.StatusCode, Method: resp.Request.Method, URL: resp.Request.URL.String(), Message: msg}
}

var linkNextRE = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(h string) string {
	if m := linkNextRE.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

// Get fetches a REST resource into v.
func (g *GitHub) Get(ctx context.Context, path string, v any) error {
	data, _, err := g.get(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// GetAll fetches every page of a REST list resource and appends the
// elements to *v, which must be a pointer to a slice.
func GetAll[T any](ctx context.Context, g *GitHub, path string) ([]T, error) {
	var all []T
	next := path
	for pages := 0; next != "" && pages < 30; pages++ {
		data, n, err := g.get(ctx, next)
		if err != nil {
			return nil, err
		}
		var page []T
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = n
	}
	return all, nil
}

// Do sends a non-GET REST request with a JSON body and decodes the
// JSON response into out, if out is not nil.
func (g *GitHub) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := g.newRequest(ctx, method, g.API+path, body)
	if err != nil {
		return err
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return apiError(resp, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// A GraphQLError is an error reported in a GraphQL response.
type GraphQLError struct {
	Messages []string
}

func (e *GraphQLError) Error() string {
	return "github graphql: " + strings.Join(e.Messages, "; ")
}

// errNoToken is returned for requests that need a token when there is none.
var errNoToken = errors.New("not logged in to GitHub (run \"gh auth login\" or set $GITHUB_TOKEN)")

// Query runs a GraphQL query or mutation and decodes its data into out.
// GraphQL needs authentication even for public data.
func (g *GitHub) Query(ctx context.Context, query string, vars map[string]any, out any) error {
	if g.Token == "" {
		return errNoToken
	}
	data, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := g.newRequest(ctx, "POST", g.GraphQL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(resp, body)
	}
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return err
	}
	if out != nil && len(r.Data) > 0 && string(r.Data) != "null" {
		if err := json.Unmarshal(r.Data, out); err != nil {
			return err
		}
	}
	if len(r.Errors) > 0 {
		ge := &GraphQLError{}
		for _, e := range r.Errors {
			ge.Messages = append(ge.Messages, e.Message)
		}
		return ge
	}
	return nil
}

// ---- REST types ----------------------------------------------------------

// A GHUser is a GitHub account.
type GHUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	Type      string `json:"type"`
}

// A GHPull is a pull request.
type GHPull struct {
	Number    int       `json:"number"`
	NodeID    string    `json:"node_id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	BodyHTML  string    `json:"body_html"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	Merged    bool      `json:"merged"`
	HTMLURL   string    `json:"html_url"`
	User      GHUser    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      GHRef     `json:"head"`
	Base      GHRef     `json:"base"`

	RequestedReviewers []GHUser `json:"requested_reviewers"`
	Additions          int      `json:"additions"`
	Deletions          int      `json:"deletions"`
	ChangedFiles       int      `json:"changed_files"`
	Comments           int      `json:"comments"`
	ReviewComments     int      `json:"review_comments"`
}

// A GHRef is one end of a pull request.
type GHRef struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
	Repo  *struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repo"`
}

// A GHTimelineEvent is one entry of an issue's timeline. Only the fields
// this program uses are decoded.
type GHTimelineEvent struct {
	Event     string     `json:"event"`
	ID        int64      `json:"id"`
	NodeID    string     `json:"node_id"`
	Actor     *GHUser    `json:"actor"`
	User      *GHUser    `json:"user"` // for "reviewed" and "commented"
	CreatedAt *time.Time `json:"created_at"`
	CommitID  string     `json:"commit_id"`

	// For "committed" events.
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Committer *struct {
		Name  string    `json:"name"`
		Email string    `json:"email"`
		Date  time.Time `json:"date"`
	} `json:"committer"`
	Author *struct {
		Name  string    `json:"name"`
		Email string    `json:"email"`
		Date  time.Time `json:"date"`
	} `json:"author"`

	// For "reviewed" events.
	SubmittedAt *time.Time `json:"submitted_at"`
	State       string     `json:"state"`
}

// A GHReviewComment is a comment on a line or file of a pull request.
type GHReviewComment struct {
	ID                int64     `json:"id"`
	NodeID            string    `json:"node_id"`
	ReviewID          int64     `json:"pull_request_review_id"`
	InReplyTo         int64     `json:"in_reply_to_id"`
	Path              string    `json:"path"`
	Line              int       `json:"line"`
	OriginalLine      int       `json:"original_line"`
	StartLine         int       `json:"start_line"`
	OriginalStartLine int       `json:"original_start_line"`
	Side              string    `json:"side"`
	StartSide         string    `json:"start_side"`
	CommitID          string    `json:"commit_id"`
	OriginalCommitID  string    `json:"original_commit_id"`
	SubjectType       string    `json:"subject_type"`
	Body              string    `json:"body"`
	BodyHTML          string    `json:"body_html"`
	User              GHUser    `json:"user"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	HTMLURL           string    `json:"html_url"`
	DiffHunk          string    `json:"diff_hunk"`
}

// A GHReview is a submitted (or pending) review.
type GHReview struct {
	ID          int64     `json:"id"`
	NodeID      string    `json:"node_id"`
	User        GHUser    `json:"user"`
	Body        string    `json:"body"`
	BodyHTML    string    `json:"body_html"`
	State       string    `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
	HTMLURL     string    `json:"html_url"`
}

// A GHIssueComment is a comment on the pull request as a whole.
type GHIssueComment struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	User      GHUser    `json:"user"`
	Body      string    `json:"body"`
	BodyHTML  string    `json:"body_html"`
	CreatedAt time.Time `json:"created_at"`
	HTMLURL   string    `json:"html_url"`
}

// A GHSearchResult is the response from the issue search API.
type GHSearchResult struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		Number        int       `json:"number"`
		Title         string    `json:"title"`
		HTMLURL       string    `json:"html_url"`
		RepositoryURL string    `json:"repository_url"`
		User          GHUser    `json:"user"`
		UpdatedAt     time.Time `json:"updated_at"`
		Comments      int       `json:"comments"`
		Draft         bool      `json:"draft"`
		State         string    `json:"state"`
	} `json:"items"`
}

// ---- GraphQL: review threads and publishing ------------------------------

// A GQLThread is what GraphQL knows about a review thread that REST does
// not: its node ID, which mutations need, and whether it is resolved.
type GQLThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Comments   struct {
		Nodes []struct {
			DatabaseID int64 `json:"databaseId"`
		} `json:"nodes"`
	} `json:"comments"`
}

// A GQLForcePush records a force push, which GraphQL describes with both
// the commit pushed over and the commit that replaced it. The REST
// timeline has only the latter, which loses a PR's very first head when
// nobody reviewed it before it was replaced.
type GQLForcePush struct {
	CreatedAt    time.Time               `json:"createdAt"`
	Actor        *struct{ Login string } `json:"actor"`
	BeforeCommit *struct{ OID string }   `json:"beforeCommit"`
	AfterCommit  *struct{ OID string }   `json:"afterCommit"`
}

// A GQLPRExtra is the part of a pull request fetched with GraphQL.
type GQLPRExtra struct {
	Threads     []GQLThread
	ForcePushes []GQLForcePush
	Viewer      string
	// PendingReview is the node ID of a review the viewer started on
	// GitHub and has not submitted, if any. GitHub allows only one.
	PendingReview string
}

const prExtraQuery = `
query($owner: String!, $repo: String!, $number: Int!, $threadsAfter: String) {
  viewer { login }
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $threadsAfter) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          isOutdated
          comments(first: 1) { nodes { databaseId } }
        }
      }
      timelineItems(first: 100, itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT]) {
        nodes {
          ... on HeadRefForcePushedEvent {
            createdAt
            actor { login }
            beforeCommit { oid }
            afterCommit { oid }
          }
        }
      }
      reviews(first: 5, states: [PENDING]) {
        nodes { id viewerDidAuthor }
      }
    }
  }
}`

// PRExtra fetches the thread and force-push details only GraphQL has.
func (g *GitHub) PRExtra(ctx context.Context, owner, repo string, number int) (*GQLPRExtra, error) {
	out := &GQLPRExtra{}
	var after any
	for page := 0; page < 20; page++ {
		var r struct {
			Viewer     struct{ Login string }
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool
							EndCursor   string
						}
						Nodes []GQLThread
					}
					TimelineItems struct {
						Nodes []GQLForcePush
					}
					Reviews struct {
						Nodes []struct {
							ID              string
							ViewerDidAuthor bool
						}
					}
				}
			}
		}
		err := g.Query(ctx, prExtraQuery, map[string]any{
			"owner": owner, "repo": repo, "number": number, "threadsAfter": after,
		}, &r)
		if err != nil {
			return nil, err
		}
		pr := r.Repository.PullRequest
		out.Viewer = r.Viewer.Login
		out.Threads = append(out.Threads, pr.ReviewThreads.Nodes...)
		if page == 0 {
			out.ForcePushes = pr.TimelineItems.Nodes
			for _, rv := range pr.Reviews.Nodes {
				if rv.ViewerDidAuthor {
					out.PendingReview = rv.ID
				}
			}
		}
		if !pr.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		after = pr.ReviewThreads.PageInfo.EndCursor
	}
	return out, nil
}

// writeFileAtomic writes data to name by way of a temporary file, so that
// a crash never leaves a half-written file behind.
func writeFileAtomic(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), name)
}
