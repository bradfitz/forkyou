// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Forkyou is a code review interface for GitHub pull requests in the
// style of Gerrit, built on rsc.io/cmd/review.
//
// Usage:
//
//	forkyou [-addr localhost:8222] [-data dir] [-open]
//
// It serves a web page for every pull request at the same path as on
// github.com, so http://localhost:8222/owner/repo/pull/123 shows
// github.com/owner/repo/pull/123. Paste any GitHub URL into the box on
// the front page, or prefix one with the server's address:
// http://localhost:8222/github.com/owner/repo/pull/123 redirects there.
//
// It authenticates to GitHub with $GITHUB_TOKEN, or else with the token
// stored by the gh command ("gh auth login"). Without a token it can
// show public pull requests, but cannot publish or resolve anything.
//
// # Snapshots
//
// Every push to a pull request, force push or not, is a snapshot,
// numbered from 1 like Gerrit's patch sets. Any two snapshots can be
// compared. When the later one has been rebased onto a newer base, the
// lines that came with the rebase are drawn in Gerrit's muted "due to
// rebase" colors, files only the rebase touched are held out of the file
// list, and the line counts include only what the pull request itself
// wrote. A git range-diff of the two is a key press away (v r).
//
// # Comments
//
// Comments are drafts until published, as in Gerrit, and are published
// together as a single GitHub review: a plain comment, an approval, or a
// request for changes. Any line can be commented on, in any snapshot,
// and so can a range of lines (click one line number, shift-click
// another) or a whole file. GitHub allows line comments only on lines in
// its own diff of the newest head; a comment anywhere else is carried
// over to the newest snapshot by the text of its line and, if GitHub
// still will not take it there, is published on the file, or in the
// review's body, quoting the lines it is about.
//
// # Storage
//
// Drafts, reviewed marks and preferences are JSON files under the data
// directory, $XDG_CONFIG_HOME/forkyou by default. Each repository's code
// is kept there in a blob-less bare clone, from which file contents are
// fetched as they are needed.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

func main() {
	addr := flag.String("addr", "localhost:8222", "address to listen on")
	data := flag.String("data", defaultDataDir(), "directory for state and caches")
	api := flag.String("api", "https://api.github.com", "GitHub REST API base URL")
	graphql := flag.String("graphql", "https://api.github.com/graphql", "GitHub GraphQL endpoint")
	gitBase := flag.String("git", "https://github.com", "base URL to clone repositories from")
	open := flag.Bool("open", false, "open a browser once listening")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("forkyou: ")

	gh := &GitHub{
		API:      *api,
		GraphQL:  *graphql,
		Token:    tokenFromGH(),
		CacheDir: filepath.Join(*data, "cache", "api"),
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
	s := newServer(gh, &Store{Dir: *data}, filepath.Join(*data, "git"), *gitBase)
	if gh.Token == "" {
		log.Printf("no GitHub token: read-only and rate-limited (run \"gh auth login\" or set $GITHUB_TOKEN)")
	} else {
		var u GHUser
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := gh.Get(ctx, "/user", &u)
		cancel()
		if err != nil {
			log.Printf("checking GitHub token: %v", err)
		}
		s.viewer = u.Login
		if s.viewer == "" {
			// A fine-grained token can lack access to /user and still
			// work for GraphQL, which can say who it belongs to.
			var r struct{ Viewer struct{ Login string } }
			if err := gh.Query(context.Background(), "{ viewer { login } }", nil, &r); err == nil {
				s.viewer = r.Viewer.Login
			}
		}
		log.Printf("logged in to GitHub as %s", s.viewer)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	u := "http://" + ln.Addr().String() + "/"
	log.Printf("serving at %s", u)
	if *open {
		openBrowser(u)
	}
	srv := &http.Server{Handler: http.NewCrossOriginProtection().Handler(s)}
	log.Fatal(srv.Serve(ln))
}

func defaultDataDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "forkyou")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "forkyou-data"
	}
	return filepath.Join(home, ".config", "forkyou")
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "forkyou: opening browser: %v\n", err)
	}
}
