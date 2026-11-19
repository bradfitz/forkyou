// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"html/template"
	"regexp"
	"strings"
)

// Commit messages name the issues they are for in trailers like
//
//	Updates #123
//	Fixes tailscale/corp#39994
//	Updates #12, #34 and tailscale/corp#56
//
// and those references are links, with a bare #123 meaning an issue in
// the pull request's own repository.

// issueRefsRE matches a keyword and the run of references after it.
var issueRefsRE = regexp.MustCompile(`(?i)\b(?:updates?|updated|fix(?:es|ed)?|close[sd]?|resolve[sd]?|see(?:\s+also)?|for|part\s+of)\b(?:[\s,:]+(?:and\s+)?(?:[\w.-]+/[\w.-]+)?#\d+\b)+`)

// issueRefRE matches one reference within such a run.
var issueRefRE = regexp.MustCompile(`(?:([\w.-]+)/([\w.-]+))?#(\d+)\b`)

// linkIssueRefs turns the issue references in s, which must already be
// HTML-escaped text, into links. Escaping leaves the characters of a
// reference alone, so they are found as typed.
func linkIssueRefs(s, owner, repo string) string {
	if !strings.Contains(s, "#") {
		return s
	}
	return issueRefsRE.ReplaceAllStringFunc(s, func(run string) string {
		return issueRefRE.ReplaceAllStringFunc(run, func(ref string) string {
			m := issueRefRE.FindStringSubmatch(ref)
			o, r := owner, repo
			if m[1] != "" {
				o, r = m[1], m[2]
			}
			return `<a class="issueref" href="https://github.com/` + o + "/" + r + "/issues/" + m[3] + `">` + ref + `</a>`
		})
	})
}

// msgHTML renders a commit message as HTML with its issue references linked.
func msgHTML(msg string, p *prPage) template.HTML {
	return template.HTML(linkIssueRefs(template.HTMLEscapeString(msg), p.Key.Owner, p.Key.Repo))
}
