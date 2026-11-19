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

**Commit by commit.** A snapshot with several commits lists them in order,
each with its own files, so a stack can be read the way it was written.
Mark files or a whole commit reviewed, and a reviewed commit folds away.
Marks are kept on what each file's change says, not on commit IDs, so they
survive the next push: when a stack is rebased or a later commit is reworked,
commits whose changes are the same come up still reviewed and folded, and the
ones that changed are flagged ("changed since snapshot 2", linking to the
range-diff). In a commit's diff, `]` and `M` carry on into the next commit,
and comments written there are published on the same line of the newest head.

**The commit message is a file.** As in Gerrit, each commit's message is
reviewed as a file named `/COMMIT_MSG`, listed first. Against the parent it
reads as new; between two snapshots it shows how the message was reworded,
as does the range-diff. Comment on its lines like any other file. GitHub has
no place for such comments, so they are published in the review's body, as
sections that read naturally on github.com (quoting the commit and line) and
carry an invisible marker that forkyou reads back, so they show up here as
threads on the message, follow the commit across rewrites, and can be
replied to and resolved. Comments on files outside the PR use the same
mechanism.

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
