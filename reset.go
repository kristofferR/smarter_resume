package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata"
)

var ampmPattern = regexp.MustCompile(`(?i)\b([0-9]{1,2})(?::([0-9]{2}))?\s*([ap])\.?m\.?\b`)
var timeOnlyPattern = regexp.MustCompile(`^[0-9]{1,2}:[0-9]{2}[AP]M$`)
var timeOnly24Pattern = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):[0-5][0-9]$`)
var tzOffsetPattern = regexp.MustCompile(`^(?:UTC|GMT)?([+-])([0-9]{1,2})(?::?([0-9]{2}))?$`)

// resetGraceWindow treats a reset time slightly in the past as "already
// lifted" instead of rolling it a full day forward — the transcript is parsed
// after claude exits, which can be minutes after the notice was printed.
const resetGraceWindow = 15 * time.Minute

// tzAbbreviations maps common abbreviations in claude's limit notices to IANA
// zones. Go's tzdata only resolves a few legacy names (EST, CET, ...) and
// those lack DST rules, so prefer real zones. IST is ambiguous
// (India/Ireland/Israel); India is by far the most common in practice.
var tzAbbreviations = map[string]string{
	"PT":   "America/Los_Angeles",
	"PST":  "America/Los_Angeles",
	"PDT":  "America/Los_Angeles",
	"MT":   "America/Denver",
	"MST":  "America/Denver",
	"MDT":  "America/Denver",
	"CT":   "America/Chicago",
	"CST":  "America/Chicago",
	"CDT":  "America/Chicago",
	"ET":   "America/New_York",
	"EST":  "America/New_York",
	"EDT":  "America/New_York",
	"AKST": "America/Anchorage",
	"AKDT": "America/Anchorage",
	"HST":  "Pacific/Honolulu",
	"BST":  "Europe/London",
	"CET":  "Europe/Paris",
	"CEST": "Europe/Paris",
	"EET":  "Europe/Helsinki",
	"EEST": "Europe/Helsinki",
	"AEST": "Australia/Sydney",
	"AEDT": "Australia/Sydney",
	"IST":  "Asia/Kolkata",
	"JST":  "Asia/Tokyo",
	"Z":    "UTC",
	"UTC":  "UTC",
	"GMT":  "UTC",
}

// resolveLocation turns whatever timezone text the limit notice carried into a
// usable location. It never fails: an unknown zone falls back to the machine's
// local zone — resuming slightly off beats never resuming, and the wake-time
// verification against the rate-limit snapshot corrects an early guess.
func resolveLocation(tz string) *time.Location {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return time.Local
	}
	if name, ok := tzAbbreviations[strings.ToUpper(tz)]; ok {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	if m := tzOffsetPattern.FindStringSubmatch(strings.ToUpper(tz)); m != nil {
		hours, _ := strconv.Atoi(m[2])
		mins := 0
		if m[3] != "" {
			mins, _ = strconv.Atoi(m[3])
		}
		offset := hours*3600 + mins*60
		if m[1] == "-" {
			offset = -offset
		}
		return time.FixedZone(tz, offset)
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.Local
}

func parseResetTime(text string, tz string, now time.Time) (time.Time, error) {
	loc := resolveLocation(tz)

	resetText := normalizeResetText(text)
	nowInLoc := now.In(loc)

	layout := ""
	switch {
	case timeOnlyPattern.MatchString(resetText):
		layout = "3:04PM"
	case timeOnly24Pattern.MatchString(resetText):
		layout = "15:04"
	}
	if layout != "" {
		tod, err := time.ParseInLocation(layout, resetText, loc)
		if err != nil {
			return time.Time{}, err
		}
		reset := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), tod.Hour(), tod.Minute(), 0, 0, loc)
		if !reset.After(nowInLoc) {
			if nowInLoc.Sub(reset) <= resetGraceWindow {
				// The reset just passed while we were parsing — it already
				// lifted, so wake immediately instead of waiting a day.
				return nowInLoc, nil
			}
			reset = reset.AddDate(0, 0, 1)
		}
		return reset, nil
	}

	type layoutCandidate struct {
		parseLayout string
		value       string
		rollYear    bool
	}

	layouts := []layoutCandidate{
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
			if nowInLoc.Sub(reset) <= resetGraceWindow {
				return nowInLoc, nil
			}
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
