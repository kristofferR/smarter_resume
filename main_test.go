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
		`{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"old notice resets 7:00pm (UTC)"}]}}`,
		`{"type":"message","content":"not a reset"}`,
		`not-json but resets 1:00pm (UTC)`,
		`{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"RESETS 9:15pm (Asia/Kolkata)"}]}}`,
		`{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"Your limit will reset at 3:00pm (America/Santiago)"}]}}`,
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

// Only assistant records flagged isApiErrorMessage may count as limit notices.
// Tool results, model prose, and any other record embedding "resets <time>"
// text killed live sessions when this repo's own source (which contains such
// strings) was read into a wrapped session.
func TestFindResetInfoIgnoresToolResultsAndProse(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := strings.Join([]string{
		// A Read tool result carrying this repo's own source code.
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"// Anchor ambiguous times (\"resets 3pm\") to the moment the notice was"}]}}`,
		`{"type":"user","toolUseResult":{"file":{"content":"limit reached - resets 3am (UTC)"}}}`,
		// The model talking about rate limits.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the 5h bucket resets 8:00pm (UTC)"}]}}`,
		// Flat legacy-looking shapes without the API-error flag.
		`{"type":"error","message":"resets 9:15pm (Asia/Kolkata)"}`,
		`{"type":"result","nested":{"text":"API limit resets Apr 14, 2026 11:30pm (America/New_York)."}}`,
		// An API error that is not a limit notice.
		`{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"API Error: 529 Overloaded"}]}}`,
		// Reset-like text outside a text block, even on a flagged record.
		`{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"tool_use","input":{"note":"resets 4:00pm (UTC)"}}]}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if info, ok, err := findResetInfo(path, 1); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("no record here is a genuine limit notice, got %#v", info)
	}
}

func TestFindResetInfoReadsGenuineLimitNotice(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	// The exact shape claude writes on a hit limit.
	content := `{"type":"assistant","isApiErrorMessage":true,"timestamp":"2026-04-13T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"You've hit your session limit · resets 3:00pm (America/Santiago)"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	info, ok, err := findResetInfo(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected genuine limit notice to be detected")
	}
	if info.Text != "3:00pm" || info.TZ != "America/Santiago" {
		t.Fatalf("unexpected reset info: %#v", info)
	}
	if want := time.Date(2026, time.April, 13, 10, 0, 0, 0, time.UTC); !info.When.Equal(want) {
		t.Fatalf("record timestamp got %s, want %s", info.When, want)
	}
}

func TestResetInfoFromTextVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		text     string
		wantText string
		wantTZ   string
		wantOK   bool
	}{
		{"parenthesized IANA", "Your limit will reset at 3:00pm (America/Santiago)", "3:00pm", "America/Santiago", true},
		{"bare abbreviation", "resets 5:30pm PST", "5:30pm", "PST", true},
		{"bare offset", "resets at 11pm UTC+2", "11pm", "UTC+2", true},
		{"no timezone", "limit reached - resets 3am", "3am", "", true},
		{"24 hour clock", "resets at 17:30", "17:30", "", true},
		{"colon separator", "reset: 6:00pm (UTC)", "6:00pm", "UTC", true},
		{"date with year", "resets Apr 14, 2026 11:30pm (America/New_York).", "Apr 14, 2026 11:30pm", "America/New_York", true},
		{"prose word not a timezone", "resets 3:00pm today", "3:00pm", "", true},
		{"no time", "the counter resets whenever", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := resetInfoFromText(tt.text)
			if ok != tt.wantOK {
				t.Fatalf("ok got %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if info.Text != tt.wantText || info.TZ != tt.wantTZ {
				t.Fatalf("got text=%q tz=%q, want text=%q tz=%q", info.Text, info.TZ, tt.wantText, tt.wantTZ)
			}
		})
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

func TestParseResetTimeExtendedFormats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC)

	got, err := parseResetTime("17:30", "UTC", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, time.April, 13, 17, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("24h got %s, want %s", got, want)
	}

	la := mustLocation(t, "America/Los_Angeles")
	got, err = parseResetTime("5pm", "PST", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, time.April, 13, 17, 0, 0, 0, la); !got.Equal(want) {
		t.Fatalf("PST got %s, want %s", got, want)
	}

	got, err = parseResetTime("3pm", "UTC+2", now)
	if err != nil {
		t.Fatal(err)
	}
	// 3pm UTC+2 == 1pm UTC on the same day.
	if want := time.Date(2026, time.April, 13, 13, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("UTC+2 got %s, want %s", got, want)
	}

	// A reset a few minutes in the past means it just lifted — wake now
	// instead of rolling a full day forward.
	got, err = parseResetTime("11:50am", "UTC", now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(now) {
		t.Fatalf("grace window got %s, want %s", got, now)
	}
}

func TestResetAfterRunDecisions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.April, 13, 18, 0, 0, 0, time.UTC)
	newCfg := func(dir string) config {
		return config{
			StateFile: filepath.Join(dir, ".rate_limits"),
			WarnFile:  filepath.Join(dir, ".rl_warn"),
			Now:       func() time.Time { return now },
		}
	}
	writeSession := func(t *testing.T, dir string, lines ...string) string {
		t.Helper()
		path := filepath.Join(dir, "session.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("stale snapshot is ignored", func(t *testing.T) {
		dir := t.TempDir()
		cfg := newCfg(dir)
		content := "written_at=" + strconvFormat(now.Add(-2*time.Hour).Unix()) +
			"\n5h_pct=100\n5h_reset=" + strconvFormat(now.Add(time.Hour).Unix()) + "\n"
		if err := os.WriteFile(cfg.StateFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		session := writeSession(t, dir, `{"type":"message","content":"no limits here"}`)

		_, ok, err := resetAfterRun(cfg, session, 1, now.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("snapshot written before the run must not trigger a resume")
		}
	})

	t.Run("transcript notice anchored to its own timestamp", func(t *testing.T) {
		dir := t.TempDir()
		cfg := newCfg(dir)
		// Notice at 10:00 saying "resets 3pm"; claude exited at 18:00 — the
		// limit lifted hours ago, the session simply continued past it.
		session := writeSession(t, dir,
			`{"type":"assistant","isApiErrorMessage":true,"timestamp":"2026-04-13T10:00:00Z","message":{"content":[{"type":"text","text":"limit reached, resets 3pm (UTC)"}]}}`)

		_, ok, err := resetAfterRun(cfg, session, 1, now.Add(-9*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("a limit that lifted long before exit must not schedule a resume")
		}
	})

	t.Run("active limit from transcript resumes at parsed time", func(t *testing.T) {
		dir := t.TempDir()
		cfg := newCfg(dir)
		session := writeSession(t, dir,
			`{"type":"assistant","isApiErrorMessage":true,"timestamp":"2026-04-13T17:55:00Z","message":{"content":[{"type":"text","text":"limit reached, resets 9pm (UTC)"}]}}`)

		reset, ok, err := resetAfterRun(cfg, session, 1, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, time.April, 13, 21, 0, 0, 0, time.UTC)
		if !ok || !reset.Equal(want) {
			t.Fatalf("got ok=%v reset=%s, want %s", ok, reset, want)
		}
	})

	t.Run("fresh exhausted snapshot wins over transcript", func(t *testing.T) {
		dir := t.TempDir()
		cfg := newCfg(dir)
		until := now.Add(45 * time.Minute)
		content := "written_at=" + strconvFormat(now.Unix()) +
			"\n5h_pct=100\n5h_reset=" + strconvFormat(until.Unix()) + "\n"
		if err := os.WriteFile(cfg.StateFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		session := writeSession(t, dir, `{"type":"message","content":"irrelevant"}`)

		reset, ok, err := resetAfterRun(cfg, session, 1, now.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if !ok || reset.Unix() != until.Unix() {
			t.Fatalf("got ok=%v reset=%d, want %d", ok, reset.Unix(), until.Unix())
		}
	})
}

func TestSessionFileMatchesCWD(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cwd := filepath.Join(string(filepath.Separator), "tmp", "some", "project")

	matching := filepath.Join(dir, "matching.jsonl")
	if err := os.WriteFile(matching, []byte(`{"type":"user","cwd":"`+strings.ReplaceAll(cwd, `\`, `\\`)+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign.jsonl")
	if err := os.WriteFile(foreign, []byte(`{"type":"user","cwd":"/somewhere/else"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noCWD := filepath.Join(dir, "nocwd.jsonl")
	if err := os.WriteFile(noCWD, []byte(`{"type":"custom-title","customTitle":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !sessionFileMatchesCWD(matching, cwd) {
		t.Fatal("matching cwd rejected")
	}
	if sessionFileMatchesCWD(foreign, cwd) {
		t.Fatal("foreign cwd accepted")
	}
	if !sessionFileMatchesCWD(noCWD, cwd) {
		t.Fatal("cwd-less transcript must be accepted")
	}
}

func TestFindLatestSessionFallbackSkipsForeignProjects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cwd := filepath.Join(string(filepath.Separator), "tmp", "some", "project")

	// No per-project dir exists, so discovery falls back to scanning all
	// projects. The newest file belongs to another project and must lose to
	// the older file that names this cwd.
	foreignDir := filepath.Join(dir, "-other-project")
	ownDir := filepath.Join(dir, "-tmp-some-project-worktree")
	for _, d := range []string{foreignDir, ownDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	foreign := filepath.Join(foreignDir, "foreign.jsonl")
	if err := os.WriteFile(foreign, []byte(`{"type":"user","cwd":"/other/project"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(ownDir, "own.jsonl")
	if err := os.WriteFile(own, []byte(`{"type":"user","cwd":"`+strings.ReplaceAll(cwd, `\`, `\\`)+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(own, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, ok, err := findLatestSessionAfter(dir, cwd, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != own {
		t.Fatalf("got ok=%v path=%q, want %q", ok, got, own)
	}
}

func TestWaitUntilLimitsLiftExtendsWhileBlocked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stderr bytes.Buffer
	cfg := config{
		StateFile: filepath.Join(dir, ".rate_limits"),
		WarnFile:  filepath.Join(dir, ".rl_warn"),
		Stderr:    &stderr,
		Now:       time.Now,
	}

	start := time.Now()
	// Unix() truncates to whole seconds, so +2s guarantees the effective
	// blocked-until instant is at least one full second in the future.
	until := start.Add(2 * time.Second)
	content := "written_at=" + strconvFormat(start.Unix()) +
		"\n5h_pct=100\n5h_reset=" + strconvFormat(until.Unix()) + "\n"
	if err := os.WriteFile(cfg.StateFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// wakeAt is already in the past; the snapshot says the limit lifts later,
	// so the wait must extend instead of resuming early.
	if !waitUntilLimitsLift(context.Background(), cfg, start, "session", start.Add(-time.Second), 0) {
		t.Fatal("expected wait to complete")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("resumed after %s despite limits blocked until +2s", elapsed)
	}
	if !strings.Contains(stderr.String(), "Limits still exhausted") {
		t.Fatalf("expected extension notice, stderr: %q", stderr.String())
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

func TestRateLimitStateBlockedUntil(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		state       rateLimitState
		wantBlocked bool
		wantUntil   time.Time
	}{
		{
			name: "approaching limit does not block",
			state: rateLimitState{
				FiveHour: rlBucket{Pct: 91, Reset: now.Add(30 * time.Minute)},
				SevenDay: rlBucket{Pct: 95, Reset: now.Add(time.Hour)},
			},
			wantBlocked: false,
		},
		{
			name: "exhausted bucket blocks until its reset",
			state: rateLimitState{
				FiveHour: rlBucket{Pct: 100, Reset: now.Add(30 * time.Minute)},
				SevenDay: rlBucket{Pct: 42, Reset: now.Add(24 * time.Hour)},
			},
			wantBlocked: true,
			wantUntil:   now.Add(30 * time.Minute),
		},
		{
			name: "both exhausted blocks until the later reset",
			state: rateLimitState{
				FiveHour: rlBucket{Pct: 100, Reset: now.Add(30 * time.Minute)},
				SevenDay: rlBucket{Pct: 100.4, Reset: now.Add(2 * time.Hour)},
			},
			wantBlocked: true,
			wantUntil:   now.Add(2 * time.Hour),
		},
		{
			name: "exhausted bucket with past reset does not block",
			state: rateLimitState{
				FiveHour: rlBucket{Pct: 100, Reset: now.Add(-time.Minute)},
			},
			wantBlocked: false,
		},
		{
			name: "seven day exhausted while five hour merely high",
			state: rateLimitState{
				FiveHour: rlBucket{Pct: 97, Reset: now.Add(30 * time.Minute)},
				SevenDay: rlBucket{Pct: 100, Reset: now.Add(48 * time.Hour)},
			},
			wantBlocked: true,
			wantUntil:   now.Add(48 * time.Hour),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			until, blocked := tt.state.blockedUntil(now)
			if blocked != tt.wantBlocked {
				t.Fatalf("blocked got %v, want %v", blocked, tt.wantBlocked)
			}
			if blocked && !until.Equal(tt.wantUntil) {
				t.Fatalf("until got %s, want %s", until, tt.wantUntil)
			}
		})
	}
}

func TestLoadRateLimitStateFallsBackToLegacyWarnFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stateFile := filepath.Join(dir, ".rate_limits")
	warnFile := filepath.Join(dir, ".rl_warn")
	now := time.Now().Unix()

	// Legacy warn file written at a 95% warning threshold: present, but must
	// NOT read as blocked — this was the premature-resume bug.
	legacy := strings.Join([]string{
		"5h_pct=95",
		"5h_reset=" + strconvFormat(now+1800),
		"7d_pct=10",
		"7d_reset=" + strconvFormat(now+3600),
	}, "\n")
	if err := os.WriteFile(warnFile, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	state, ok, err := loadRateLimitState(stateFile, warnFile)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected legacy warn file to load")
	}
	if _, blocked := state.blockedUntil(time.Now()); blocked {
		t.Fatal("95% warning threshold must not count as a limit hit")
	}
	if !state.fresh(time.Now().Add(-time.Minute)) {
		t.Fatal("legacy file mtime should count as written-at")
	}

	// The state file wins over the warn file once present.
	current := strings.Join([]string{
		"written_at=" + strconvFormat(now),
		"5h_pct=100",
		"5h_reset=" + strconvFormat(now+1800),
		"7d_pct=10",
		"7d_reset=" + strconvFormat(now+3600),
	}, "\n")
	if err := os.WriteFile(stateFile, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}

	state, ok, err = loadRateLimitState(stateFile, warnFile)
	if err != nil {
		t.Fatal(err)
	}
	until, blocked := state.blockedUntil(time.Now())
	if !ok || !blocked || until.Unix() != now+1800 {
		t.Fatalf("state file got ok=%v blocked=%v until=%d, want blocked until %d", ok, blocked, until.Unix(), now+1800)
	}
	if state.fresh(time.Now().Add(time.Hour)) {
		t.Fatal("snapshot written before the run must not be fresh")
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
	if cfg.StateFile != filepath.Join(claudeConfigDir, ".rate_limits") {
		t.Fatalf("StateFile got %q", cfg.StateFile)
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
printf '5h_pct=100\n5h_reset=%s\n7d_pct=10\n7d_reset=%s\n' "$((now + 60))" "$((now + 3600))" > "$WARN_FILE"
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
mkdir -p "$session_dir" "$(dirname "$RL_STATE")"
session="$session_dir/$SESSION_ID.jsonl"
if [ "$count" -eq 1 ]; then
  printf '{"type":"message","content":"first"}\n' > "$session"
  now=$(date +%s)
  printf 'written_at=%s\n5h_pct=100\n5h_reset=%s\n7d_pct=10\n7d_reset=0\n' "$now" "$((now + 3))" > "$RL_STATE"
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
		"RL_STATE="+cfg.StateFile,
		"ARGS_LOG="+argsLog,
		"STATE_FILE="+stateFile,
		"SESSION_ID="+sessionID,
	)

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
		"--dangerously-skip-permissions --settings global-settings.json --settings settings.json --model sonnet --resume " + sessionID + " " + resumeMessage,
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args got %#v, want %#v", gotArgs, wantArgs)
	}

	sessionFile := filepath.Join(projectsDir, "fake-project", sessionID+".jsonl")
	title, ok, err := latestSessionTitle(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !strings.HasPrefix(title, "rl-") {
		t.Fatalf("expected generated title, ok=%v title=%q", ok, title)
	}

	// Non-TTY writers must never receive ANSI escapes.
	if strings.Contains(stderr.String(), "\033") {
		t.Fatalf("stderr contains ANSI escapes: %q", stderr.String())
	}
}

func TestRunDoesNotResumeAtWarningThreshold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	warnFile := filepath.Join(dir, ".claude", ".rl_warn")
	argsLog := filepath.Join(dir, "args.log")
	sessionID := "22222222-2222-2222-2222-222222222222"
	script := writeScript(t, dir, "fake-claude", `#!/bin/sh
printf '%s\n' "$*" >> "$ARGS_LOG"
session_dir="$PROJECTS_DIR/fake-project"
mkdir -p "$session_dir" "$(dirname "$RL_STATE")"
printf '{"type":"message","content":"worked fine"}\n' > "$session_dir/$SESSION_ID.jsonl"
now=$(date +%s)
printf 'written_at=%s\n5h_pct=91.2\n5h_reset=%s\n7d_pct=97\n7d_reset=%s\n' "$now" "$((now + 1800))" "$((now + 86400))" > "$RL_STATE"
exit 0
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := testConfig(script, projectsDir, warnFile, &stdout, &stderr)
	cfg.Env = append(os.Environ(),
		"PROJECTS_DIR="+projectsDir,
		"RL_STATE="+cfg.StateFile,
		"ARGS_LOG="+argsLog,
		"SESSION_ID="+sessionID,
	)

	code := run(context.Background(), cfg, []string{"--model", "sonnet"})
	if code != 0 {
		t.Fatalf("exit code got %d, want 0", code)
	}
	if got := readLines(t, argsLog); len(got) != 1 {
		t.Fatalf("expected a single launch, got %d: %#v", len(got), got)
	}
	if strings.Contains(stderr.String(), "Rate limit hit") {
		t.Fatalf("high-but-not-exhausted usage must not schedule a resume, stderr: %q", stderr.String())
	}
}

// A limit hit mid-run must not kill claude immediately — background shells
// die with it. The watcher waits the limit out while claude keeps running and
// restarts it only once the limits lifted.
func TestRunRestartsInteractiveClaudeAfterLimitLifts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	warnFile := filepath.Join(dir, ".claude", ".rl_warn")
	argsLog := filepath.Join(dir, "args.log")
	stateFile := filepath.Join(dir, "state")
	sessionID := "33333333-3333-3333-3333-333333333333"
	// Mimics claude's interactive TUI: the first Ctrl-C only cancels input,
	// a second one within the grace window exits.
	script := writeScript(t, dir, "fake-claude", `#!/bin/sh
count=0
if [ -f "$STATE_FILE" ]; then
  count=$(cat "$STATE_FILE")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$STATE_FILE"
printf '%s %s\n' "$(date +%s)" "$*" >> "$ARGS_LOG"
session_dir="$PROJECTS_DIR/fake-project"
mkdir -p "$session_dir" "$(dirname "$RL_STATE")"
session="$session_dir/$SESSION_ID.jsonl"
if [ "$count" -eq 1 ]; then
  printf '{"type":"message","content":"first"}\n' > "$session"
  ints=0
  trap 'ints=$((ints + 1)); if [ "$ints" -ge 2 ]; then exit 130; fi' INT
  now=$(date +%s)
  printf 'written_at=%s\n5h_pct=100\n5h_reset=%s\n7d_pct=10\n7d_reset=0\n' "$now" "$((now + 3))" > "$RL_STATE"
  while :; do sleep 0.05; done
fi
printf '{"type":"message","content":"second"}\n' >> "$session"
exit 0
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := testConfig(script, projectsDir, warnFile, &stdout, &stderr)
	cfg.Env = append(os.Environ(),
		"PROJECTS_DIR="+projectsDir,
		"RL_STATE="+cfg.StateFile,
		"ARGS_LOG="+argsLog,
		"STATE_FILE="+stateFile,
		"SESSION_ID="+sessionID,
	)

	code := run(context.Background(), cfg, []string{"--model", "sonnet"})
	if code != 0 {
		t.Fatalf("exit code got %d, want 0 after auto-resume, stderr: %q", code, stderr.String())
	}
	launches := readLines(t, argsLog)
	if len(launches) != 2 {
		t.Fatalf("expected launch + resume, got %d launches: %#v", len(launches), launches)
	}
	// The restart must not happen before the reset epoch the stub announced.
	resumedAt := launchEpoch(t, launches[1])
	if reset := stateFileEpoch(t, cfg.StateFile, "5h_reset"); resumedAt < reset {
		t.Fatalf("claude was restarted at %d, before the limit lifted at %d", resumedAt, reset)
	}
	if !strings.Contains(stderr.String(), "Rate limit lifted") {
		t.Fatalf("expected deferred-restart notice, stderr: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "Rate limit hit") {
		t.Fatalf("countdown banner must not print while claude handles the wait, stderr: %q", stderr.String())
	}
}

// A snapshot detection that lands before the run's session file exists must
// stay pending — not enter the wait with an empty path, which would freeze
// discovery, skip the stall check, and restart a session that continued after
// the reset on its own.
func TestRunHoldsRestartUntilSessionFileResolved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	warnFile := filepath.Join(dir, ".claude", ".rl_warn")
	argsLog := filepath.Join(dir, "args.log")
	sessionID := "55555555-5555-5555-5555-555555555555"
	// Blocked snapshot exists from the start; the transcript only appears
	// after the reset has passed, carrying post-reset activity.
	script := writeScript(t, dir, "fake-claude", `#!/bin/sh
printf '%s\n' "$*" >> "$ARGS_LOG"
mkdir -p "$(dirname "$RL_STATE")"
now=$(date +%s)
printf 'written_at=%s\n5h_pct=100\n5h_reset=%s\n7d_pct=10\n7d_reset=0\n' "$now" "$((now + 2))" > "$RL_STATE"
sleep 3
session_dir="$PROJECTS_DIR/fake-project"
mkdir -p "$session_dir"
printf '{"type":"message","content":"continued after reset"}\n' > "$session_dir/$SESSION_ID.jsonl"
exit 0
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := testConfig(script, projectsDir, warnFile, &stdout, &stderr)
	cfg.Env = append(os.Environ(),
		"PROJECTS_DIR="+projectsDir,
		"RL_STATE="+cfg.StateFile,
		"ARGS_LOG="+argsLog,
		"SESSION_ID="+sessionID,
	)

	code := run(context.Background(), cfg, []string{"--model", "sonnet"})
	if code != 0 {
		t.Fatalf("exit code got %d, want 0, stderr: %q", code, stderr.String())
	}
	if got := readLines(t, argsLog); len(got) != 1 {
		t.Fatalf("session continued past the reset and must not be restarted, got %d launches: %#v", len(got), got)
	}
}

// waitOutLimit must only green-light a restart for a stalled session:
// transcript writes after the reset mean the session already continued.
func TestWaitOutLimitSkipsRestartWhenSessionContinued(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config{
		StateFile: filepath.Join(dir, ".rate_limits"),
		WarnFile:  filepath.Join(dir, ".rl_warn"),
		Now:       time.Now,
	}
	session := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resetAt := time.Now().Add(-time.Minute)
	if waitOutLimit(context.Background(), cfg, resetAt, time.Now().Add(-time.Hour), session) {
		t.Fatal("post-reset transcript activity must not trigger a restart")
	}

	stale := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(session, stale, stale); err != nil {
		t.Fatal(err)
	}
	if !waitOutLimit(context.Background(), cfg, resetAt, time.Now().Add(-time.Hour), session) {
		t.Fatal("stalled session must be restarted once the limit lifted")
	}
}

func TestRunKillsClaudeThatIgnoresSignals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	warnFile := filepath.Join(dir, ".claude", ".rl_warn")
	argsLog := filepath.Join(dir, "args.log")
	stateFile := filepath.Join(dir, "state")
	sessionID := "44444444-4444-4444-4444-444444444444"
	script := writeScript(t, dir, "fake-claude", `#!/bin/sh
count=0
if [ -f "$STATE_FILE" ]; then
  count=$(cat "$STATE_FILE")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$STATE_FILE"
printf '%s\n' "$*" >> "$ARGS_LOG"
session_dir="$PROJECTS_DIR/fake-project"
mkdir -p "$session_dir" "$(dirname "$RL_STATE")"
session="$session_dir/$SESSION_ID.jsonl"
if [ "$count" -eq 1 ]; then
  printf '{"type":"message","content":"first"}\n' > "$session"
  trap '' INT TERM
  now=$(date +%s)
  printf 'written_at=%s\n5h_pct=100\n5h_reset=%s\n7d_pct=10\n7d_reset=0\n' "$now" "$((now + 3))" > "$RL_STATE"
  while :; do sleep 0.05; done
fi
printf '{"type":"message","content":"second"}\n' >> "$session"
exit 0
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := testConfig(script, projectsDir, warnFile, &stdout, &stderr)
	cfg.InterruptRepeat = 20 * time.Millisecond
	cfg.InterruptGrace = 50 * time.Millisecond
	cfg.TermGrace = 50 * time.Millisecond
	cfg.Env = append(os.Environ(),
		"PROJECTS_DIR="+projectsDir,
		"RL_STATE="+cfg.StateFile,
		"ARGS_LOG="+argsLog,
		"STATE_FILE="+stateFile,
		"SESSION_ID="+sessionID,
	)

	code := run(context.Background(), cfg, []string{"--model", "sonnet"})
	if code != 0 {
		t.Fatalf("exit code got %d, want 0 after auto-resume, stderr: %q", code, stderr.String())
	}
	if got := readLines(t, argsLog); len(got) != 2 {
		t.Fatalf("expected launch + resume, got %d launches: %#v", len(got), got)
	}
}

func TestFindClaudeBinSkipsCmuxShim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	shimDir := filepath.Join(root, "T", "cmux-cli-shims", "ABC")
	realDir := filepath.Join(root, ".local", "bin")
	for _, d := range []string{shimDir, realDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	shim := writeScript(t, shimDir, "claude", "#!/bin/sh\nexit 0\n")
	real := writeScript(t, realDir, "claude", "#!/bin/sh\nexit 0\n")

	// Shim directory comes first in PATH, mirroring the real failure.
	pathEnv := strings.Join([]string{shimDir, realDir}, string(os.PathListSeparator))
	got, err := findClaudeBin(pathEnv, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("findClaudeBin got %q, want real claude %q (shim was %q)", got, real, shim)
	}
}

func TestFindCmuxShim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	shimDir := filepath.Join(root, "cmux-cli-shims", "XYZ")
	otherDir := filepath.Join(root, "bin")
	for _, d := range []string{shimDir, otherDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	shim := writeScript(t, shimDir, "claude", "#!/bin/sh\nexit 0\n")
	writeScript(t, otherDir, "claude", "#!/bin/sh\nexit 0\n")
	pathEnv := strings.Join([]string{otherDir, shimDir}, string(os.PathListSeparator))

	if got := findCmuxShim("", pathEnv); got != shim {
		t.Fatalf("findCmuxShim(PATH) got %q, want %q", got, shim)
	}
	if got := findCmuxShim(shim, ""); got != shim {
		t.Fatalf("findCmuxShim(pinned) got %q, want %q", got, shim)
	}
	if got := findCmuxShim("", otherDir); got != "" {
		t.Fatalf("findCmuxShim without a shim dir got %q, want empty", got)
	}
}

func TestArgsHaveCmuxHooks(t *testing.T) {
	t.Parallel()
	hooks := `{"hooks":{"SessionStart":[{"hooks":[{"command":"cmux hooks claude session-start"}]}]}}`
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"separate flag", []string{"--settings", hooks, "--model", "sonnet"}, true},
		{"inline flag", []string{"--settings=" + hooks}, true},
		{"user settings file", []string{"--settings", "my-settings.json"}, false},
		{"no settings", []string{"--dangerously-skip-permissions"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argsHaveCmuxHooks(tc.args); got != tc.want {
				t.Fatalf("argsHaveCmuxHooks(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestResolveLaunch(t *testing.T) {
	t.Parallel()
	hooks := `{"hooks":{"Stop":[{"hooks":[{"command":"cmux hooks feed --source claude"}]}]}}`
	base := config{ClaudeBin: "/real/claude", Env: []string{"PATH=/bin", "FOO=bar"}}

	t.Run("no shim runs real claude", func(t *testing.T) {
		bin, env := resolveLaunch(base, []string{"--dangerously-skip-permissions"})
		if bin != base.ClaudeBin {
			t.Fatalf("bin got %q, want %q", bin, base.ClaudeBin)
		}
		if !reflect.DeepEqual(env, base.Env) {
			t.Fatalf("env got %v, want %v", env, base.Env)
		}
	})

	t.Run("cmux already wrapped passes through to real claude", func(t *testing.T) {
		cfg := base
		cfg.CmuxShim = "/cmux/shim/claude"
		bin, env := resolveLaunch(cfg, []string{"--settings", hooks})
		if bin != cfg.ClaudeBin {
			t.Fatalf("bin got %q, want real claude %q", bin, cfg.ClaudeBin)
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "CMUX_CUSTOM_CLAUDE_PATH=") {
				t.Fatalf("did not expect CMUX_CUSTOM_CLAUDE_PATH when already wrapped: %q", kv)
			}
		}
	})

	t.Run("self-wrap routes through shim pinned to real claude", func(t *testing.T) {
		cfg := base
		cfg.CmuxShim = "/cmux/shim/claude"
		bin, env := resolveLaunch(cfg, []string{"--dangerously-skip-permissions"})
		if bin != cfg.CmuxShim {
			t.Fatalf("bin got %q, want shim %q", bin, cfg.CmuxShim)
		}
		want := "CMUX_CUSTOM_CLAUDE_PATH=" + cfg.ClaudeBin
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("env %v missing %q", env, want)
		}
	})
}

func TestSetEnvVarReplacesExisting(t *testing.T) {
	t.Parallel()
	env := []string{"A=1", "CMUX_CUSTOM_CLAUDE_PATH=/old", "B=2"}
	got := setEnvVar(env, "CMUX_CUSTOM_CLAUDE_PATH", "/new")
	want := []string{"A=1", "CMUX_CUSTOM_CLAUDE_PATH=/new", "B=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("setEnvVar got %v, want %v", got, want)
	}
	// Original slice must be untouched.
	if env[1] != "CMUX_CUSTOM_CLAUDE_PATH=/old" {
		t.Fatalf("setEnvVar mutated input: %v", env)
	}
	got = setEnvVar([]string{"A=1"}, "NEW", "x")
	if want := []string{"A=1", "NEW=x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("setEnvVar append got %v, want %v", got, want)
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
		ClaudeBin:           claudeBin,
		ProjectsDir:         projectsDir,
		WarnFile:            warnFile,
		StateFile:           warnFile + ".rate_limits",
		Buffer:              0,
		WatchInterval:       10 * time.Millisecond,
		InterruptRepeat:     200 * time.Millisecond,
		InterruptGrace:      5 * time.Second,
		TermGrace:           2 * time.Second,
		RelaunchGuardWindow: 2 * time.Minute,
		Stdin:               strings.NewReader(""),
		Stdout:              stdout,
		Stderr:              stderr,
		Env:                 os.Environ(),
		Now:                 time.Now,
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

// launchEpoch reads the leading `date +%s` stamp a fake-claude script wrote to
// its args log line.
func launchEpoch(t *testing.T, logLine string) int64 {
	t.Helper()
	field, _, _ := strings.Cut(logLine, " ")
	n, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		t.Fatalf("launch log line %q has no leading epoch: %v", logLine, err)
	}
	return n
}

// stateFileEpoch reads one epoch value from a key=value rate-limit snapshot.
func stateFileEpoch(t *testing.T, path string, key string) int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || k != key {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("state %s=%q: %v", key, v, err)
		}
		return n
	}
	t.Fatalf("state file %s missing %s", path, key)
	return 0
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
