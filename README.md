# smarter_resume

Cross-platform rewrite of [Smart Resume for Claude Code](https://github.com/kristofferR/smart_resume)
as a single Go binary.

It replaces the three divergent Bash/Zsh wrapper scripts (Linux / WSL / macOS)
with one statically-linked binary — no `python3` dependency, real JSON +
timezone parsing, first-class signal handling, and platform-independent tests.

**Status:** first Go implementation. See
[#1](https://github.com/kristofferR/smarter_resume/issues/1) for the design and
acceptance criteria.

## What it does

Wraps the real `claude` binary: runs your session normally, detects when a
usage limit is hit, waits until the limit lifts (plus a buffer), and
auto-resumes the same session — looping until Claude exits for any other
reason.

### How limits are detected

The primary signal is a **rate-limit snapshot** written by the statusline
script to `$CLAUDE_CONFIG_DIR/.rate_limits` on every render. Claude Code hands
the statusline exact utilization percentages and reset epochs for the 5-hour
and 7-day buckets, so the wrapper never has to guess:

```
written_at=1720612345
5h_pct=87.3
5h_reset=1720612800
7d_pct=42.0
7d_reset=1721030400
```

A bucket counts as blocking only when its utilization reached **100%** and its
reset epoch is still in the future; the wrapper waits until every blocking
bucket has reset. Snapshots written before the current run started are
ignored. The legacy `.rl_warn` sentinel is still read (with the same
100%-threshold semantics) when no snapshot exists.

When no usable snapshot is available, the wrapper falls back to scanning the
session transcript for the "resets ..." notice. Only assistant records flagged
`isApiErrorMessage` — the shape Claude Code actually writes on a hit limit —
are considered, and only their message content is searched. Tool results, file
contents, and ordinary model prose can never match, so reading or discussing
text like "resets 3pm" does not trigger a false limit. 12/24-hour times and
bare timezone abbreviations (`PST`, `CEST`, `UTC+2`, ...) are understood, and
ambiguous times are anchored to the notice's own timestamp so a limit that was
already waited out never triggers a spurious resume.

### Reliability behavior

- While Claude runs, a watcher polls the snapshot and the transcript. When a
  limit is hit it does **not** kill Claude — background shells and tasks keep
  running through the wait. Once the limits have verifiably lifted (and the
  transcript shows the session is still stalled), it restarts Claude with an
  escalating sequence (SIGINT ×2 — the interactive TUI needs two Ctrl-C — then
  SIGTERM, then SIGKILL) and immediately resumes the session so work continues.
- Before resuming, the wrapper re-checks the snapshot; if the limits have not
  actually lifted, it extends the wait instead of resuming early.
- If a resume re-hits the limit right away, an escalating extra buffer (up to
  15 minutes) prevents tight relaunch loops.
- Session discovery verifies the transcript's `cwd` field in fallback paths so
  a concurrent session in another project is never adopted.
- Progress output is plain text when stderr is not a terminal (no ANSI codes
  in logs or pipes).

The binary keeps the shell wrapper's behavior while removing the platform split:

- passes args, stdio, and non-rate-limit exit codes through to the real `claude`
- finds the latest Claude JSONL session for the current project
- parses JSONL with Go's JSON decoder instead of grep/sed
- parses reset times with Go's time-zone database instead of `date` or `python3`
- resumes the same session with `claude --resume <session-id>`

## Configuration

Environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `CLAUDE_BIN` | first real `claude` in `PATH` | Path to the Claude Code binary being wrapped |
| `CLAUDE_CONFIG_DIR` | `$HOME/.claude` | Claude local data directory |
| `PROJECTS_DIR` | `$CLAUDE_CONFIG_DIR/projects` | Claude JSONL transcript directory |
| `BUFFER_SECS` | `60` | Extra wait after the reset time |
| `CLAUDE_SMART_RESUME_CONFIG` | `$HOME/.config/smarter_resume/settings.json` | Path to Smart Resume settings |
| `CLAUDE_SMART_RESUME_SKIP_PERMISSIONS` | unset | When truthy, prepends `--dangerously-skip-permissions` to the first run |
| `CLAUDE_SMART_RESUME_WATCH_SECS` | `5` | Snapshot/transcript watcher poll interval |

The statusline script must write the `.rate_limits` snapshot for the primary
detection path; see "How limits are detected" above for the format. Without
it, the wrapper still works via transcript parsing, just with fewer
guarantees.

Smart Resume can also prepend default Claude args to every wrapped session.
Those args are preserved when the wrapper auto-resumes the session:

```json
{
  "defaultArgs": ["--dangerously-skip-permissions"]
}
```

Save that as `$HOME/.config/smarter_resume/settings.json` to run all wrapped
Claude sessions, including automatic resumes, with that flag. The alias
`claudeArgs` is also accepted for the same list.

## Build

```sh
go build ./...
```

Cross-compile the supported targets:

```sh
GOOS=linux GOARCH=amd64 go build -o dist/smarter_resume_linux_amd64 .
GOOS=linux GOARCH=arm64 go build -o dist/smarter_resume_linux_arm64 .
GOOS=darwin GOARCH=amd64 go build -o dist/smarter_resume_darwin_amd64 .
GOOS=darwin GOARCH=arm64 go build -o dist/smarter_resume_darwin_arm64 .
```

## Test

```sh
go test ./...
```

## Install

`install.sh` downloads the matching release asset for linux/darwin amd64/arm64
and installs it atomically to `$HOME/.claude/smarter_resume`:

```sh
./install.sh
```

Then alias it:

```sh
alias claude="$HOME/.claude/smarter_resume"
```

Release archives are built by GitHub Actions when a `v*` tag is pushed. Asset
names match the installer contract, for example
`smarter_resume_darwin_arm64.tar.gz`.

## License

MIT (matching upstream).
