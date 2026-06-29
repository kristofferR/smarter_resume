package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata"
)

var ampmPattern = regexp.MustCompile(`(?i)\b([0-9]{1,2})(?::([0-9]{2}))?\s*([ap])\.?m\.?\b`)
var timeOnlyPattern = regexp.MustCompile(`^[0-9]{1,2}:[0-9]{2}[AP]M$`)

func parseResetTime(text string, tz string, now time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone %q: %w", tz, err)
	}

	resetText := normalizeResetText(text)
	nowInLoc := now.In(loc)

	if timeOnlyPattern.MatchString(resetText) {
		tod, err := time.ParseInLocation("3:04PM", resetText, loc)
		if err != nil {
			return time.Time{}, err
		}
		reset := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), tod.Hour(), tod.Minute(), 0, 0, loc)
		if !reset.After(nowInLoc) {
			reset = reset.AddDate(0, 0, 1)
		}
		return reset, nil
	}

	type layout struct {
		parseLayout string
		value       string
		rollYear    bool
	}

	layouts := []layout{
		{parseLayout: "Jan 2, 2006 3:04PM", value: resetText},
		{parseLayout: "Jan 2 2006 3:04PM", value: resetText},
		{parseLayout: "January 2, 2006 3:04PM", value: resetText},
		{parseLayout: "January 2 2006 3:04PM", value: resetText},
		{parseLayout: "2006 Jan 2, 3:04PM", value: fmt.Sprintf("%d %s", nowInLoc.Year(), resetText), rollYear: true},
		{parseLayout: "2006 Jan 2 3:04PM", value: fmt.Sprintf("%d %s", nowInLoc.Year(), resetText), rollYear: true},
		{parseLayout: "2006 January 2, 3:04PM", value: fmt.Sprintf("%d %s", nowInLoc.Year(), resetText), rollYear: true},
		{parseLayout: "2006 January 2 3:04PM", value: fmt.Sprintf("%d %s", nowInLoc.Year(), resetText), rollYear: true},
	}

	var lastErr error
	for _, candidate := range layouts {
		reset, err := time.ParseInLocation(candidate.parseLayout, candidate.value, loc)
		if err != nil {
			lastErr = err
			continue
		}
		if !reset.After(nowInLoc) && candidate.rollYear {
			reset = reset.AddDate(1, 0, 0)
		}
		if !reset.After(nowInLoc) {
			return time.Time{}, fmt.Errorf("reset time %q is not in the future", text)
		}
		return reset, nil
	}

	if lastErr == nil {
		lastErr = errors.New("unsupported reset time")
	}
	return time.Time{}, lastErr
}

func normalizeResetText(text string) string {
	collapsed := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	return ampmPattern.ReplaceAllStringFunc(collapsed, func(match string) string {
		parts := ampmPattern.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		minute := parts[2]
		if minute == "" {
			minute = "00"
		}
		return parts[1] + ":" + minute + strings.ToUpper(parts[3]) + "M"
	})
}

func resetFromWarnFile(path string) (time.Time, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	defer f.Close()

	values := map[string]int64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			continue
		}
		values[strings.TrimSpace(key)] = n
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, false, err
	}

	pct5h := values["5h_pct"]
	pct7d := values["7d_pct"]
	reset5h := values["5h_reset"]
	reset7d := values["7d_reset"]

	var epoch int64
	switch {
	case pct5h > pct7d:
		epoch = reset5h
	case pct7d > pct5h:
		epoch = reset7d
	case reset7d > reset5h:
		epoch = reset7d
	default:
		epoch = reset5h
	}
	if epoch <= 0 {
		return time.Time{}, false, nil
	}
	return time.Unix(epoch, 0), true, nil
}
