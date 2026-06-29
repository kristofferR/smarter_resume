package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFindResetInfoUsesJSONAndStartLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := strings.Join([]string{
		`{"type":"error","message":"old reset resets 7:00pm (UTC)"}`,
		`{"type":"message","content":"not a reset"}`,
		`{"type":"result","nested":{"text":"API limit resets Apr 14, 2026 11:30pm (America/New_York)."}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ordinary content says resets 8:00pm (UTC)"}]}}`,
		`not-json but resets 1:00pm (UTC)`,
		`{"type":"error","message":"RESETS 9:15pm (Asia/Kolkata)"}`,
		`{"type":"error","message":"Your limit will reset at 3:00pm (America/Santiago)"}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	info, ok, err := findResetInfo(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected reset info")
	}
	if info.Text != "3:00pm" || info.TZ != "America/Santiago" {
		t.Fatalf("unexpected reset info: %#v", info)
	}
}

func TestSessionTitleAndAppendRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := strings.Join([]string{
		`{"type":"custom-title","customTitle":"first","sessionId":"abc"}`,
		`{"type":"message","content":"hello"}`,
		`{"type":"custom-title","customTitle":"name \"with\" quotes","sessionId":"abc"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	title, ok, err := latestSessionTitle(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || title != `name "with" quotes` {
		t.Fatalf("unexpected title ok=%v title=%q", ok, title)
	}

	if err := appendSessionTitle(path, "abc", `path\to\thing`); err != nil {
		t.Fatal(err)
	}
	title, ok, err = latestSessionTitle(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || title != `path\to\thing` {
		t.Fatalf("unexpected appended title ok=%v title=%q", ok, title)
	}
}

func TestParseResetTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		text string
		tz   string
		want time.Time
	}{
		{
			name: "time only today",
			text: "11:59pm",
			tz:   "UTC",
			want: time.Date(2026, time.April, 13, 23, 59, 0, 0, time.UTC),
		},
		{
			name: "time only rolls over",
			text: "12:01AM",
			tz:   "UTC",
			want: time.Date(2026, time.April, 14, 0, 1, 0, 0, time.UTC),
		},
		{
			name: "hour only today",
			text: "6pm",
			tz:   "UTC",
			want: time.Date(2026, time.April, 13, 18, 0, 0, 0, time.UTC),
		},
		{
			name: "hour only rolls over",
			text: "2am",
			tz:   "UTC",
			want: time.Date(2026, time.April, 14, 2, 0, 0, 0, time.UTC),
		},
		{
			name: "date without year",
			text: "Apr 14 7:30pm",
			tz:   "UTC",
			want: time.Date(2026, time.April, 14, 19, 30, 0, 0, time.UTC),
		},
		{
			name: "full date with zone",
			text: "Apr 13, 2099 11:59pm",
			tz:   "America/New_York",
			want: time.Date(2099, time.April, 13, 23, 59, 0, 0, mustLocation(t, "America/New_York")),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResetTime(tt.text, tt.tz, now)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}

	if _, err := parseResetTime("Jan 1, 2020 12:00am", "UTC", now); err == nil {
		t.Fatal("expected past full date to fail")
	}
	if _, err := parseResetTime("not-a-time", "UTC", now); err == nil {
		t.Fatal("expected invalid time to fail")
	}
}

func TestParseResetTimeRollsYearlessDatesIntoNextYear(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.December, 31, 23, 30, 0, 0, time.UTC)
	got, err := parseResetTime("Jan 1 12:30am", "UTC", now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2027, time.January, 1, 0, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestFindLatestSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cwd := filepath.Join(string(filepath.Separator), "tmp", "some", "project")
	encoded := encodedCWD(cwd)
	if encoded != "-tmp-some-project" {
		t.Fatalf("encoded cwd got %q, want %q", encoded, "-tmp-some-project")
	}
	specialCWD := filepath.Join(string(filepath.Separator), "Users", "me", "Dev Tooling", "smarter_resume", "Små ting")
	if got, want := encodedCWD(specialCWD), "-Users-me-Dev-Tooling-smarter-resume-Sm-ting"; got != want {
		t.Fatalf("encoded special cwd got %q, want %q", got, want)
	}
	sessionDir := filepath.Join(dir, encoded)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}

	oldFile := filepath.Join(sessionDir, "old.jsonl")
	newFile := filepath.Join(sessionDir, "new.jsonl")
	if err := os.WriteFile(oldFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newFile, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	got, ok, err := findLatestSession(dir, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != newFile {
		t.Fatalf("got ok=%v path=%q, want %q", ok, got, newFile)
	}

	worktreeDir := filepath.Join(dir, "-tmp-some-project-worktree-feature")
	if err := os.MkdirAll(worktreeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	worktreeFile := filepath.Join(worktreeDir, "worktree.jsonl")
	if err := os.WriteFile(worktreeFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktreeTime := newTime.Add(time.Hour)
	if err := os.Chtimes(worktreeFile, worktreeTime, worktreeTime); err != nil {
		t.Fatal(err)
	}

	got, ok, err = findLatestSessionAfter(dir, cwd, newTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != worktreeFile {
		t.Fatalf("current-run fallback got ok=%v path=%q, want %q", ok, got, worktreeFile)
	}

	fallbackDir := t.TempDir()
	nested := filepath.Join(fallbackDir, "other")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	fallbackFile := filepath.Join(nested, "session.jsonl")
	if err := os.WriteFile(fallbackFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err = findLatestSession(fallbackDir, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != fallbackFile {
		t.Fatalf("fallback got ok=%v path=%q, want %q", ok, got, fallbackFile)
	}
}

func TestFindRunSessionFallsBackToGrownExistingSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cwd := filepath.Join(string(filepath.Separator), "tmp", "some", "project")
	sessionDir := filepath.Join(dir, encodedCWD(cwd))
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}

	sessionFile := filepath.Join(sessionDir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(sessionFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(sessionFile, []byte("{}\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sessionFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	got, ok, err := findRunSession(dir, cwd, oldTime.Add(time.Hour), sessionFile, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != sessionFile {
		t.Fatalf("got ok=%v path=%q, want %q", ok, got, sessionFile)
	}
}

func TestWarnFileSelection(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".rl_warn")
	now := time.Now().Unix()
	content := strings.Join([]string{
		"5h_pct=80",
		"5h_reset=" + strconvFormat(now+1800),
		"7d_pct=95",
		"7d_reset=" + strconvFormat(now+3600),
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err := resetFromWarnFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Unix() != now+3600 {
		t.Fatalf("got ok=%v epoch=%d, want %d", ok, got.Unix(), now+3600)
	}

	tiePath := filepath.Join(t.TempDir(), ".rl_warn")
	tieContent := strings.Join([]string{
		"5h_pct=100",
		"5h_reset=" + strconvFormat(now+1800),
		"7d_pct=100",
		"7d_reset=" + strconvFormat(now+7200),
	}, "\n")
	if err := os.WriteFile(tiePath, []byte(tieContent), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err = resetFromWarnFile(tiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Unix() != now+7200 {
		t.Fatalf("tie got ok=%v epoch=%d, want %d", ok, got.Unix(), now+7200)
	}
}

func TestLoadConfigHonorsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	claudeConfigDir := filepath.Join(t.TempDir(), "claude-config")

	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BIN", "/bin/true")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	t.Setenv("PROJECTS_DIR", "")
	t.Setenv("CLAUDE_SMART_RESUME_CONFIG", "")
	t.Setenv("BUFFER_SECS", "")
	t.Setenv("CLAUDE_SMART_RESUME_WATCH_SECS", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectsDir != filepath.Join(claudeConfigDir, "projects") {
		t.Fatalf("ProjectsDir got %q", cfg.ProjectsDir)
	}
	if cfg.WarnFile != filepath.Join(claudeConfigDir, ".rl_warn") {
		t.Fatalf("WarnFile got %q", cfg.WarnFile)
	}
}

func TestGenerateSessionName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.April, 13, 0, 0, 0, 0, time.UTC)
	got := generateSessionName(now, "/Users/Kristoffer/Dev Tooling/smarter_resume")
	want := "rl-2026-04-13-dev-tooling-smarter-resume"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRunPassesArgsAndReturnsExitCode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.log")
	script := writeScript(t, dir, "fake-claude", `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_FILE"
printf 'stdout ok\n'
printf 'stderr ok\n' >&2
exit 7
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := testConfig(script, filepath.Join(dir, "projects"), filepath.Join(dir, ".claude", ".rl_warn"), &stdout, &stderr)
	cfg.Env = append(os.Environ(), "ARGS_FILE="+argsFile)

	code := run(context.Background(), cfg, []string{"--foo", "bar"})
	if code != 7 {
		t.Fatalf("exit code got %d, want 7", code)
	}
	if !strings.Contains(stdout.String(), "stdout ok") {
		t.Fatalf("stdout not forwarded: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "stderr ok") {
		t.Fatalf("stderr not forwarded: %q", stderr.String())
	}

	gotArgs := readLines(t, argsFile)
	if want := []string{"--foo", "bar"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args got %#v, want %#v", gotArgs, want)
	}
}

func TestRunReturnsNonZeroWhenWaitCancelled(t *testing.T) {
	t.Parallel()
	if got := cancelledWaitExitCode(7); got != 7 {
		t.Fatalf("cancelled wait should preserve non-zero child exit, got %d", got)
	}

	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	warnFile := filepath.Join(dir, ".claude", ".rl_warn")
	sessionID := "11111111-1111-1111-1111-111111111111"
	script := writeScript(t, dir, "fake-claude", `#!/bin/sh
session_dir="$PROJECTS_DIR/fake-project"
mkdir -p "$session_dir" "$(dirname "$WARN_FILE")"
printf '{"type":"message","message":"started"}\n' > "$session_dir/$SESSION_ID.jsonl"
now=$(date +%s)
printf '5h_pct=95\n5h_reset=%s\n7d_pct=10\n7d_reset=%s\n' "$((now + 60))" "$((now + 3600))" > "$WARN_FILE"
exit 0
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := testConfig(script, projectsDir, warnFile, &stdout, &stderr)
	cfg.Env = append(os.Environ(),
		"PROJECTS_DIR="+projectsDir,
		"WARN_FILE="+warnFile,
		"SESSION_ID="+sessionID,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancelStderr := &cancelOnWrite{needle: "Press Ctrl-C to cancel", cancel: cancel}
	cfg.Stderr = cancelStderr

	code := run(ctx, cfg, nil)
	if code != 130 {
		t.Fatalf("exit code got %d, want 130", code)
	}
}

func TestLoadSettingsArgs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"defaultArgs":["--dangerously-skip-permissions","--settings","claude.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadSettingsArgs(path, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dangerously-skip-permissions", "--settings", "claude.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args got %#v, want %#v", got, want)
	}
}

func TestInitialArgsDeduplicatesBooleanDefaults(t *testing.T) {
	t.Parallel()

	got := initialArgs(
		[]string{"--dangerously-skip-permissions"},
		[]string{"--dangerously-skip-permissions", "--model", "sonnet"},
		true,
	)
	want := []string{"--dangerously-skip-permissions", "--model", "sonnet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args got %#v, want %#v", got, want)
	}
}

func TestResumeArgsPreserveStartupContextWithoutReplayingPrompt(t *testing.T) {
	t.Parallel()

	original := []string{
		"--dangerously-skip-permissions",
		"--dangerously-skip-permissions",
		"--settings", "settings.json",
		"--session-id=11111111-1111-1111-1111-111111111111",
		"--model", "sonnet",
		"--system-prompt-file", "system.md",
		"--append-system-prompt-file=append.md",
		"--permission-mode", "auto",
		"--add-dir", "../shared", "../docs",
		"--debug", "api",
		"--print",
		"--output-format", "json",
		"do not replay me",
	}

	got := resumeArgs(original, "11111111-1111-1111-1111-111111111111")
	want := []string{
		"--dangerously-skip-permissions",
		"--settings", "settings.json",
		"--session-id=11111111-1111-1111-1111-111111111111",
		"--model", "sonnet",
		"--system-prompt-file", "system.md",
		"--append-system-prompt-file=append.md",
		"--permission-mode", "auto",
		"--add-dir", "../shared", "../docs",
		"--debug", "api",
		"--resume", "11111111-1111-1111-1111-111111111111", resumeMessage,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args got %#v, want %#v", got, want)
	}
}

func TestResumeArgsTreatSingleValueFlagsAsSingleValue(t *testing.T) {
	t.Parallel()

	got := resumeArgs([]string{
		"--tools", "Read",
		"--mcp-config", "./mcp.json",
		"fix the failing test",
	}, "11111111-1111-1111-1111-111111111111")
	want := []string{
		"--tools", "Read",
		"--mcp-config", "./mcp.json",
		"--resume", "11111111-1111-1111-1111-111111111111", resumeMessage,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args got %#v, want %#v", got, want)
	}
}

func TestShouldForwardInterruptSkipsSameProcessGroup(t *testing.T) {
	t.Parallel()
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if shouldForwardInterrupt(process) {
		t.Fatal("expected current process group interrupt not to be forwarded")
	}
}

func TestRunAutoResumesSameSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	warnFile := filepath.Join(dir, ".claude", ".rl_warn")
	argsLog := filepath.Join(dir, "args.log")
	stateFile := filepath.Join(dir, "state")
	sessionID := "11111111-1111-1111-1111-111111111111"
	script := writeScript(t, dir, "fake-claude", `#!/bin/sh
count=0
if [ -f "$STATE_FILE" ]; then
  count=$(cat "$STATE_FILE")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$STATE_FILE"
printf '%s\n' "$*" >> "$ARGS_LOG"
session_dir="$PROJECTS_DIR/fake-project"
mkdir -p "$session_dir" "$(dirname "$WARN_FILE")"
session="$session_dir/$SESSION_ID.jsonl"
if [ "$count" -eq 1 ]; then
  printf '{"type":"message","content":"first"}\n' > "$session"
  now=$(date +%s)
  printf '5h_pct=95\n5h_reset=%s\n7d_pct=10\n7d_reset=%s\n' "$now" "$now" > "$WARN_FILE"
  exit 0
fi
printf '{"type":"message","content":"second"}\n' >> "$session"
exit 3
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := testConfig(script, projectsDir, warnFile, &stdout, &stderr)
	cfg.DefaultArgs = []string{"--dangerously-skip-permissions", "--settings", "global-settings.json"}
	cfg.Env = append(os.Environ(),
		"PROJECTS_DIR="+projectsDir,
		"WARN_FILE="+warnFile,
		"ARGS_LOG="+argsLog,
		"STATE_FILE="+stateFile,
		"SESSION_ID="+sessionID,
	)
	cfg.Now = func() time.Time {
		return time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC)
	}

	code := run(context.Background(), cfg, []string{
		"--settings", "settings.json",
		"--session-id", sessionID,
		"--model", "sonnet",
		"--print",
		"original prompt",
	})
	if code != 3 {
		t.Fatalf("exit code got %d, want 3", code)
	}

	gotArgs := readLines(t, argsLog)
	wantArgs := []string{
		"--dangerously-skip-permissions --settings global-settings.json --settings settings.json --session-id " + sessionID + " --model sonnet --print original prompt",
		"--dangerously-skip-permissions --settings global-settings.json --settings settings.json --session-id " + sessionID + " --model sonnet --resume " + sessionID + " " + resumeMessage,
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args got %#v, want %#v", gotArgs, wantArgs)
	}

	sessionFile := filepath.Join(projectsDir, "fake-project", sessionID+".jsonl")
	title, ok, err := latestSessionTitle(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !strings.HasPrefix(title, "rl-2026-04-13-") {
		t.Fatalf("expected generated title, ok=%v title=%q", ok, title)
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func strconvFormat(n int64) string {
	return strconv.FormatInt(n, 10)
}

func writeScript(t *testing.T, dir string, name string, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func testConfig(claudeBin string, projectsDir string, warnFile string, stdout *bytes.Buffer, stderr *bytes.Buffer) config {
	return config{
		ClaudeBin:     claudeBin,
		ProjectsDir:   projectsDir,
		WarnFile:      warnFile,
		Buffer:        0,
		WatchInterval: 10 * time.Millisecond,
		WatchTimeout:  50 * time.Millisecond,
		WatchSettle:   0,
		Stdin:         strings.NewReader(""),
		Stdout:        stdout,
		Stderr:        stderr,
		Env:           os.Environ(),
		Now:           time.Now,
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

type cancelOnWrite struct {
	bytes.Buffer
	mu     sync.Mutex
	once   sync.Once
	needle string
	cancel context.CancelFunc
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.Buffer.Write(p)
	if strings.Contains(string(p), w.needle) {
		w.once.Do(w.cancel)
	}
	return n, err
}
