# forkyou

A fast, Gerrit-style code review interface for GitHub pull requests.
It runs as a small local server and uses the GitHub API behind the scenes,
so everything you publish lands on GitHub as ordinary reviews and comments.

It is built on [rsc.io/cmd/review](https://pkg.go.dev/rsc.io/cmd/review):
its diff engine, its "due to rebase" detection, its templates, styles, and
Gerrit keyboard shortcuts.

## Why

I'm sick of GitHub constantly making their web UI slower, worse, and
spammier. I don't want your "New Experience ✨"; I want Gerrit. I want
snapshots. I want range diffs. I want to compare any rev against any rev.
I want speed.

Thanks to GitHub for giving me the t-shirt and naming inspiration:

![Brad in a "fork you" GitHub shirt, next to a "go fmt yourself" shirt](doc/fork-you-go-fmt-yourself-shirts.jpeg)

## Running

    go install github.com/bradfitz/forkyou@latest
    forkyou -open

It logs in with `$GITHUB_TOKEN` or, failing that, `gh auth token`. Then open
any pull request at the same path as on GitHub:

    http://localhost:8222/tailscale/tailscale/pull/12345

You can also paste a GitHub URL into the box at the top of any page, or put
the server's address in front of one:
`http://localhost:8222/github.com/owner/repo/pull/123`.

## What it does

**Snapshots.** Every push to a pull request, force push or not, is a
numbered snapshot, like a Gerrit patch set. They are rebuilt from GitHub's
own records (commits, force pushes, and the heads reviews were written
against), so everyone sees the same numbers. Pick any two to compare.

**Rebase-aware diffs.** When the newer snapshot was rebased onto a newer
base, lines that came along with the rebase are muted (Gerrit's lavender and
tan), files only the rebase touched are hidden from the file list, and line
counts include only the pull request's own edits. `v r` (or the "range-diff"
link) shows `git range-diff` of the two snapshots.

**Comments anywhere.** Comment on any line of any file in any snapshot,
on either side, on a range (click a line number, then shift-click another,
or select text and press `c`), or on a whole file. Comments are drafts until
you press Reply (`a`) and publish them as one review: Comment, Approve, or
Request changes.

GitHub accepts line comments only on lines within its own diff of the newest
head. Drafts written against an older snapshot are carried over to the
newest one by the text of their line. Anything GitHub still won't take as a
line comment becomes a file comment quoting the lines, or a quote in the
review body if the file isn't in the PR at all. Nothing gets dropped.

**Resolving** a thread takes effect on GitHub immediately, as on github.com.
Replies can also be marked to resolve when published. "Done" drafts a "Done"
reply that resolves the thread.

**Reviewed marks.** Mark files or whole snapshots reviewed. A file's mark
carries over to later snapshots in which it didn't change. Once a snapshot is
marked reviewed, later visits default to showing what changed since then.

**Keyboard.** Gerrit's shortcuts. Press `?` for the list.

## Design notes

- Pages are rendered completely on the server: no spinners, no reflow,
  no stream of background requests. Requests happen only when you act
  (open a comment form, save a draft, mark something reviewed), and each one
  returns the HTML for whatever it changed.
- Code comes from a local blob-less bare clone of each repository (under
  the data directory), from which file contents are fetched in batches as
  needed. GitHub serves commits by ID even after a force push has dropped
  them, so old snapshots stay viewable.
- GitHub REST responses are cached on disk with their ETags, so reloading
  an unchanged pull request costs 304s that don't count against the rate
  limit. Thread resolution and publishing use GraphQL.
- Local state (drafts, reviewed marks, preferences) is plain JSON files
  under `$XDG_CONFIG_HOME/forkyou` (default `~/.config/forkyou`).
