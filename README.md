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

Wraps the real `claude` binary: runs your session normally, and when Claude
exits because a rate limit was hit, it parses the exact reset time from the
session transcript, waits until then (plus a buffer), and auto-resumes the same
session — looping until Claude exits for any other reason.

The binary keeps the shell wrapper's behavior while removing the platform split:

- passes args, stdio, and non-rate-limit exit codes through to the real `claude`
- finds the latest Claude JSONL session for the current project
- parses JSONL with Go's JSON decoder instead of grep/sed
- parses reset times with Go's time-zone database instead of `date` or `python3`
- watches newly-written transcript lines and interrupts Claude when a reset
  entry appears
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
| `CLAUDE_SMART_RESUME_WATCH_SECS` | `5` | Transcript watcher poll interval |

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
