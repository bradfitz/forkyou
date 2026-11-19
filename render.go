// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Much of this file comes from rsc.io/cmd/review.

package main

import (
	"embed"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed tmpl/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"since":       since,
	"kbd":         kbdHTML,
	"keyMap":      keyMapJSON,
	"keySections": func() []keySection { return keyHelp },
	"plural": func(n int, word string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, word)
		}
		return fmt.Sprintf("%d %ss", n, word)
	},
	"short": shortRev,
	"stateClass": func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, "_", ""))
	},
	"stateText": func(s string) string {
		switch s {
		case "APPROVED":
			return "approved"
		case "CHANGES_REQUESTED":
			return "requested changes"
		case "COMMENTED":
			return "commented"
		case "DISMISSED":
			return "review dismissed"
		}
		return strings.ToLower(s)
	},
	"html_safe": func(s string) template.HTML { return template.HTML(s) },
	"msgHTML":   msgHTML,
	"sub":       func(a, b int) int { return a - b },
	"snapArg": func(p *filesPage, s *snapRow) map[string]any {
		return map[string]any{"URL": p.X("snapreviewed") + "?n=" + strconv.Itoa(s.N), "Reviewed": s.Reviewed}
	},
	"reviewedArg": func(p *prPage, file string, on, rebaseOnly bool) map[string]any {
		return map[string]any{"URL": p.X("reviewed") + p.Query("f", file), "Reviewed": on, "RebaseOnly": rebaseOnly}
	},
	"date": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
}).ParseFS(tmplFS, "tmpl/*.html"))

// since formats a time as a short relative duration, as Gerrit does.
func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("Jan 2, 2006")
}

// kbdHTML renders a binding's keys for the help dialog.
func kbdHTML(keys []string) template.HTML {
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(` <span class="or">or</span> `)
		}
		for j, part := range strings.Split(k, " ") {
			if j > 0 {
				b.WriteString(" then ")
			}
			part = strings.ReplaceAll(part, "Mod+", "Ctrl/⌘+")
			fmt.Fprintf(&b, "<kbd>%s</kbd>", template.HTMLEscapeString(part))
		}
	}
	return template.HTML(b.String())
}

// A renderRow is a diff row ready for the template.
type renderRow struct {
	Row
	LeftHTML     template.HTML
	RightHTML    template.HTML
	LeftThreads  []*Thread
	RightThreads []*Thread
	ExpandURL    string
}

// RowClass is what the row's <tr> carries: its kind, plus a mark on a row
// showing nothing but what a rebase brought, so that the keyboard can step
// over it. A row with a line of the change's own on either side is not one
// of those, however much of the rest of it came from below.
func (r *renderRow) RowClass() string {
	own := r.L.Num > 0 && !r.RebasedL || r.R.Num > 0 && !r.RebasedR
	if r.Kind != RowEqual && r.Kind != RowSkip && !own {
		return r.Class() + " rebased"
	}
	return r.Class()
}

// Class returns the CSS class for the row's kind.
func (r *renderRow) Class() string {
	switch r.Kind {
	case RowReplace:
		return "replace"
	case RowDelete:
		return "delete"
	case RowInsert:
		return "insert"
	case RowSkip:
		return "skip"
	}
	return "equal"
}

// LeftClass and RightClass give the highlight classes Gerrit uses: "remove"
// and "add" for the pale line background, plus "total" when the chunk only
// adds or only removes and the strong color covers the whole line.
func (r *renderRow) LeftClass() string {
	return sideClass(r, "remove", r.Kind == RowReplace || r.Kind == RowDelete, r.RebasedL)
}

func (r *renderRow) RightClass() string {
	return sideClass(r, "add", r.Kind == RowReplace || r.Kind == RowInsert, r.RebasedR)
}

func sideClass(r *renderRow, name string, changed, rebased bool) string {
	if !changed {
		return ""
	}
	c := name
	if r.Total {
		c += " total"
	}
	if r.NoIntraline {
		c += " nointra"
	}
	if rebased {
		c += " rebased"
	}
	return c
}

// lineHTML renders one line of a diff, marking the byte ranges that changed
// within the line and any trailing whitespace. If link is not nil, it is
// applied to each run of escaped text, to add links.
func lineHTML(text string, spans []Span, link func(string) string) template.HTML {
	if text == "" {
		return ""
	}
	// Trailing whitespace gets its own marker, as in Gerrit.
	trail := len(text)
	for trail > 0 && (text[trail-1] == ' ' || text[trail-1] == '\t') {
		trail--
	}
	if trail == len(text) {
		trail = -1
	}

	cuts := map[int]bool{0: true, len(text): true}
	for _, sp := range spans {
		cuts[sp.Lo] = true
		cuts[sp.Hi] = true
	}
	if trail >= 0 {
		cuts[trail] = true
	}
	at := make([]int, 0, len(cuts))
	for c := range cuts {
		if 0 <= c && c <= len(text) {
			at = append(at, c)
		}
	}
	sort.Ints(at)

	var b strings.Builder
	for i := 0; i+1 < len(at); i++ {
		lo, hi := at[i], at[i+1]
		if lo == hi {
			continue
		}
		var class string
		if inSpans(spans, lo) {
			class = "i"
		}
		if trail >= 0 && lo >= trail {
			if class != "" {
				class += " "
			}
			class += "ws"
		}
		seg := template.HTMLEscapeString(text[lo:hi])
		if link != nil {
			seg = link(seg)
		}
		if class == "" {
			b.WriteString(seg)
			continue
		}
		fmt.Fprintf(&b, `<span class="%s">%s</span>`, class, seg)
	}
	return template.HTML(b.String())
}

func inSpans(spans []Span, i int) bool {
	for _, sp := range spans {
		if sp.Lo <= i && i < sp.Hi {
			return true
		}
	}
	return false
}

// rangeDiffHTML colors the output of git range-diff: the header line of
// each commit pair by whether the pair is equal, changed, added or
// dropped, and inside the diff of diffs, the lines by what happened to
// them. range-diff indents a diff of the two patches by four spaces; its
// first column says whether the line is in the old patch only, the new
// patch only, or both, and the second is the patch's own marker.
func rangeDiffHTML(out string, link func(string) string) template.HTML {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		class := ""
		switch {
		case !strings.HasPrefix(line, "    "):
			class = "rdhead"
			switch {
			case strings.Contains(line, " = "):
				class += " rdsame"
			case strings.Contains(line, " ! "):
				class += " rdchanged"
			case strings.Contains(line, " < "):
				class += " rddropped"
			case strings.Contains(line, " > "):
				class += " rdadded"
			}
		case strings.HasPrefix(line, "    @@"):
			class = "rdhunk"
		case len(line) > 4 && line[4] == '-':
			class = "rdold"
		case len(line) > 4 && line[4] == '+':
			class = "rdnew"
		case len(line) > 5 && line[5] == '+':
			class = "rdctxadd"
		case len(line) > 5 && line[5] == '-':
			class = "rdctxdel"
		}
		esc := template.HTMLEscapeString(line)
		if link != nil {
			esc = link(esc)
		}
		if class == "" {
			b.WriteString(`<div class="rdline">` + esc + "\n</div>")
		} else {
			fmt.Fprintf(&b, `<div class="rdline %s">%s`+"\n</div>", class, esc)
		}
	}
	return template.HTML(b.String())
}
