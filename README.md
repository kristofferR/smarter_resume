# smarter_resume

Cross-platform rewrite of [Smart Resume for Claude Code](https://github.com/kristofferR/smart_resume)
as a single Go binary.

It replaces the three divergent Bash/Zsh wrapper scripts (Linux / WSL / macOS)
with one statically-linked binary — no `python3` dependency, real JSON +
timezone parsing, first-class signal handling, and platform-independent tests.

**Status:** scaffolding. See [#1](https://github.com/kristofferR/smarter_resume/issues/1)
for the design and acceptance criteria.

## What it does

Wraps the real `claude` binary: runs your session normally, and when Claude
exits because a rate limit was hit, it parses the exact reset time from the
session transcript, waits until then (plus a buffer), and auto-resumes the same
session — looping until Claude exits for any other reason.

## Build

```sh
go build ./...
```

## License

MIT (matching upstream).
