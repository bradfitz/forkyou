// Copyright 2026 The Forkyou Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "testing"

func TestLinkIssueRefs(t *testing.T) {
	link := func(o, r, n string) string {
		return `<a class="issueref" href="https://github.com/` + o + "/" + r + "/issues/" + n + `">`
	}
	for _, tt := range []struct{ in, want string }{
		{"Updates #123", "Updates " + link("acme", "widget", "123") + "#123</a>"},
		{"Fixes tailscale/corp#39994", "Fixes " + link("tailscale", "corp", "39994") + "tailscale/corp#39994</a>"},
		{"Closes #1, #2 and other.repo/x_y#3",
			"Closes " + link("acme", "widget", "1") + "#1</a>, " + link("acme", "widget", "2") + "#2</a> and " +
				link("other.repo", "x_y", "3") + "other.repo/x_y#3</a>"},
		{"updates #7", "updates " + link("acme", "widget", "7") + "#7</a>"},
		// Not after a keyword, not linked: #1 in code or prose means
		// anything, and "issue #5" is ambiguous enough to leave alone.
		{"use #define FOO", "use #define FOO"},
		{"commit #1 of 3", "commit #1 of 3"},
		{"Prefixes #3", "Prefixes #3"},
	} {
		if got := linkIssueRefs(tt.in, "acme", "widget"); got != tt.want {
			t.Errorf("linkIssueRefs(%q)\n got %s\nwant %s", tt.in, got, tt.want)
		}
	}
}
