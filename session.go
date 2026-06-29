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
}

var resetPattern = regexp.MustCompile(`(?i)\breset(?:s)?(?:\s+at)?\s+([^(]+?)\s*\(([^)]+)\)`)
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
		path, ok, err := newestJSONLAfter(sessionDir, 1, minModTime)
		if err != nil || ok {
			return path, ok, err
		}
	}

	return newestJSONLAfter(projectsDir, 2, minModTime)
}

func newestJSONL(root string, maxDepth int) (string, bool, error) {
	return newestJSONLAfter(root, maxDepth, time.Time{})
}

func newestJSONLAfter(root string, maxDepth int, minModTime time.Time) (string, bool, error) {
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
		if newest == "" || info.ModTime().After(newestMod) {
			newest = path
			newestMod = info.ModTime()
		}
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

func resetInfoFromJSONLine(line []byte) (resetInfo, bool) {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return resetInfo{}, false
	}

	var latest resetInfo
	found := false
	for _, s := range resetCandidateStrings(obj) {
		if info, ok := resetInfoFromText(s); ok {
			latest = info
			found = true
		}
	}
	return latest, found
}

func resetCandidateStrings(obj map[string]any) []string {
	recordType, _ := obj["type"].(string)
	if recordType == "user" || recordType == "assistant" {
		return nil
	}

	keys := []string{"message", "error", "status", "stderr"}
	if recordType == "error" || recordType == "system" || recordType == "result" {
		keys = append(keys, "content", "text")
	}

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := obj[key].(string); ok {
			out = append(out, value)
		}
	}
	return out
}

func resetInfoFromText(text string) (resetInfo, bool) {
	matches := resetPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return resetInfo{}, false
	}
	last := matches[len(matches)-1]
	return resetInfo{
		Text: strings.TrimSpace(last[1]),
		TZ:   strings.TrimSpace(last[2]),
	}, true
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
