package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type resetInfo struct {
	Text string
	TZ   string
	// When is the timestamp of the transcript record carrying the limit
	// notice, when the record has one. Zero otherwise.
	When time.Time
}

// resetPattern extracts "resets <time>" phrases: an optional date, a 12- or
// 24-hour time, and a timezone that may be parenthesized ("(America/Oslo)"),
// a bare abbreviation ("PST", case-sensitive so prose words never match), an
// offset ("UTC+2"), or absent entirely (the machine's local zone is assumed).
var resetPattern = regexp.MustCompile(
	`(?i)\breset(?:s)?(?:\s+at)?[:\s]+` +
		`((?:[A-Za-z]{3,9}\.?\s+\d{1,2}(?:,\s*\d{4})?,?\s+)?` + // optional date
		`(?:\d{1,2}(?::\d{2})?\s*[ap]\.?m\.?|\d{1,2}:\d{2}))` + // 12h or 24h time
		`\s*(?:\(([^)]+)\)|((?-i:(?:UTC|GMT)[+-]\d{1,2}(?::\d{2})?|[A-Z]{2,5}))\b)?`)

var claudeProjectSlugPattern = regexp.MustCompile(`[^A-Za-z0-9]+`)

func encodedCWD(cwd string) string {
	slash := filepath.ToSlash(filepath.Clean(cwd))
	return claudeProjectSlugPattern.ReplaceAllString(slash, "-")
}

func findLatestSession(projectsDir string, cwd string) (string, bool, error) {
	return findLatestSessionAfter(projectsDir, cwd, time.Time{})
}

func findLatestSessionAfter(projectsDir string, cwd string, minModTime time.Time) (string, bool, error) {
	sessionDir := filepath.Join(projectsDir, encodedCWD(cwd))
	if info, err := os.Stat(sessionDir); err == nil && info.IsDir() {
		path, ok, err := newestJSONLAfter(sessionDir, 1, minModTime, nil)
		if err != nil || ok {
			return path, ok, err
		}
	}

	// The fallback scans every project dir (worktrees encode to different
	// slugs), so a concurrent session in an unrelated project can be the
	// newest file — require the transcript to name this run's cwd.
	return newestJSONLAfter(projectsDir, 2, minModTime, func(path string) bool {
		return sessionFileMatchesCWD(path, cwd)
	})
}

// sessionFileMatchesCWD reports whether the transcript at path belongs to a
// session started in cwd. Claude records a top-level "cwd" field on most
// entries; files that never name a cwd within the first records are accepted
// so synthetic or trimmed transcripts keep working.
func sessionFileMatchesCWD(path string, cwd string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	want := filepath.Clean(cwd)
	reader := bufio.NewReader(f)
	for i := 0; i < 25; i++ {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var obj struct {
				CWD string `json:"cwd"`
			}
			if json.Unmarshal(line, &obj) == nil && obj.CWD != "" {
				return filepath.Clean(obj.CWD) == want
			}
		}
		if err != nil {
			return true
		}
	}
	return true
}

func newestJSONLAfter(root string, maxDepth int, minModTime time.Time, match func(string) bool) (string, bool, error) {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if !info.IsDir() {
		return "", false, nil
	}

	var newest string
	var newestMod time.Time
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == root {
			return nil
		}

		depth := pathDepth(root, path)
		if d.IsDir() {
			if depth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}

		if depth > maxDepth || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !minModTime.IsZero() && info.ModTime().Before(minModTime) {
			return nil
		}
		if newest != "" && !info.ModTime().After(newestMod) {
			return nil
		}
		if match != nil && !match(path) {
			return nil
		}
		newest = path
		newestMod = info.ModTime()
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return newest, newest != "", nil
}

func pathDepth(root string, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(filepath.ToSlash(rel), "/"))
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	lines := 0
	for {
		part, err := reader.ReadBytes('\n')
		if len(part) > 0 {
			lines++
		}
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return lines, err
		}
	}
}

func latestSessionTitle(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	defer f.Close()

	var title string
	found := false
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var obj map[string]any
			if json.Unmarshal(line, &obj) == nil && obj["type"] == "custom-title" {
				if value, ok := obj["customTitle"].(string); ok {
					title = value
					found = true
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return title, found, nil
		}
		if err != nil {
			return title, found, err
		}
	}
}

func appendSessionTitle(path string, sessionID string, title string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	entry := map[string]string{
		"type":        "custom-title",
		"customTitle": title,
		"sessionId":   sessionID,
	}
	enc := json.NewEncoder(f)
	return enc.Encode(entry)
}

func findResetInfo(path string, startLine int) (resetInfo, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resetInfo{}, false, nil
		}
		return resetInfo{}, false, err
	}
	defer f.Close()

	if startLine < 1 {
		startLine = 1
	}

	var latest resetInfo
	found := false
	lineNo := 0
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			if lineNo >= startLine && len(strings.TrimSpace(string(line))) > 0 {
				if info, ok := resetInfoFromJSONLine(line); ok {
					latest = info
					found = true
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return latest, found, nil
		}
		if err != nil {
			return latest, found, err
		}
	}
}

// resetInfoFromJSONLine extracts a limit notice from a transcript record.
// Only assistant records flagged isApiErrorMessage carry genuine notices
// (e.g. "You've hit your session limit · resets 7:10pm (Europe/Oslo)"), and
// only their message content is scanned. Nothing else may ever match:
// transcripts embed tool results and model prose, so a read file or a
// conversation that merely mentions "resets 3pm" must not read as a limit
// hit — that false positive killed live sessions and scheduled day-long waits.
func resetInfoFromJSONLine(line []byte) (resetInfo, bool) {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return resetInfo{}, false
	}
	if obj["type"] != "assistant" || obj["isApiErrorMessage"] != true {
		return resetInfo{}, false
	}
	msg, _ := obj["message"].(map[string]any)
	if msg == nil {
		return resetInfo{}, false
	}

	var latest resetInfo
	found := false
	for _, s := range messageTexts(msg["content"]) {
		if info, ok := resetInfoFromText(s); ok {
			latest = info
			found = true
		}
	}
	if found {
		latest.When = recordTimestamp(obj)
	}
	return latest, found
}

// messageTexts extracts the scannable text of a message content value: a flat
// string, or the "text" fields of typed text blocks. Other block fields (tool
// payloads and the like) are never scanned.
func messageTexts(content any) []string {
	switch t := content.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, block := range t {
			m, ok := block.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if s, ok := m["text"].(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func recordTimestamp(obj map[string]any) time.Time {
	raw, _ := obj["timestamp"].(string)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	return time.Time{}
}

func resetInfoFromText(text string) (resetInfo, bool) {
	matches := resetPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return resetInfo{}, false
	}
	last := matches[len(matches)-1]
	tz := strings.TrimSpace(last[2])
	if tz == "" {
		tz = strings.TrimSpace(last[3])
	}
	return resetInfo{
		Text: strings.TrimSpace(last[1]),
		TZ:   tz,
	}, true
}

// sessionContinuedAfter reports whether the transcript, from startLine on,
// contains genuine progress recorded after resetAt: assistant output that is
// not an API error, or a typed user prompt. Ambient writes — tool results
// landing from background tasks, records without timestamps, error notices —
// must not read as the session having moved on, or a background task
// finishing during the wake buffer would cancel the restart and leave a
// stalled session unresumed.
func sessionContinuedAfter(path string, startLine int, resetAt time.Time) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	if startLine < 1 {
		startLine = 1
	}

	lineNo := 0
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			if lineNo >= startLine && recordShowsProgress(line, resetAt) {
				return true
			}
		}
		if err != nil {
			return false
		}
	}
}

// recordShowsProgress reports whether a transcript record is post-reset
// session progress: successful assistant output, or a user prompt (not a
// tool result) — in both cases timestamped after resetAt.
func recordShowsProgress(line []byte, resetAt time.Time) bool {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return false
	}
	ts := recordTimestamp(obj)
	if ts.IsZero() || !ts.After(resetAt) {
		return false
	}
	msg, _ := obj["message"].(map[string]any)
	if msg == nil {
		return false
	}
	switch obj["type"] {
	case "assistant":
		return obj["isApiErrorMessage"] != true
	case "user":
		// Typed prompts carry string content or text blocks; tool results
		// arrive as tool_result blocks, which messageTexts ignores.
		return len(messageTexts(msg["content"])) > 0
	}
	return false
}

func generateSessionName(now time.Time, cwd string) string {
	slash := filepath.ToSlash(filepath.Clean(cwd))
	parts := strings.FieldsFunc(slash, func(r rune) bool {
		return r == '/'
	})
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}

	slug := slugify(strings.Join(parts, "-"))
	if slug == "" {
		slug = "session"
	}
	return "rl-" + now.Format("2006-01-02") + "-" + slug
}

func slugify(input string) string {
	input = strings.ToLower(input)
	var b strings.Builder
	lastDash := false
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
