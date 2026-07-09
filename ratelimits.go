package main

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// blockedPctThreshold is the utilization at which a bucket actually blocks
// requests. The statusline snapshot is written on every render, so values
// below this (e.g. 90%) mean "approaching", not "hit".
const blockedPctThreshold = 100.0

// writtenAtSlack absorbs whole-second truncation in the statusline's
// `date +%s` stamp when comparing against the run start time.
const writtenAtSlack = 2 * time.Second

type rlBucket struct {
	Pct   float64
	Reset time.Time // zero when the snapshot carried no epoch
}

type rateLimitState struct {
	WrittenAt time.Time
	FiveHour  rlBucket
	SevenDay  rlBucket
}

// readRateLimitState parses a key=value rate-limit snapshot (floats allowed).
// Unparsable lines are skipped so a partially written or older-format file
// degrades to zero values instead of an error.
func readRateLimitState(path string) (rateLimitState, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rateLimitState{}, false, nil
		}
		return rateLimitState{}, false, err
	}
	defer f.Close()

	values := map[string]float64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, raw, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			continue
		}
		values[strings.TrimSpace(key)] = n
	}
	if err := scanner.Err(); err != nil {
		return rateLimitState{}, false, err
	}

	state := rateLimitState{
		FiveHour: rlBucket{Pct: values["5h_pct"], Reset: epochTime(values["5h_reset"])},
		SevenDay: rlBucket{Pct: values["7d_pct"], Reset: epochTime(values["7d_reset"])},
	}
	if writtenAt := epochTime(values["written_at"]); !writtenAt.IsZero() {
		state.WrittenAt = writtenAt
	} else if info, err := f.Stat(); err == nil {
		// Legacy .rl_warn files carry no written_at stamp; the file mtime is
		// equivalent because the wrapper deletes that file at run start.
		state.WrittenAt = info.ModTime()
	}
	return state, true, nil
}

func epochTime(v float64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(v), 0)
}

// loadRateLimitState prefers the always-written state file and falls back to
// the legacy warn file so older statusline versions keep working.
func loadRateLimitState(stateFile string, warnFile string) (rateLimitState, bool, error) {
	if state, ok, err := readRateLimitState(stateFile); err != nil || ok {
		return state, ok, err
	}
	return readRateLimitState(warnFile)
}

// blockedUntil reports whether any bucket is exhausted (utilization at the
// limit with a reset still in the future) and, if so, the moment every
// blocking bucket has reset.
func (s rateLimitState) blockedUntil(now time.Time) (time.Time, bool) {
	var until time.Time
	blocked := false
	for _, b := range []rlBucket{s.FiveHour, s.SevenDay} {
		if b.Pct < blockedPctThreshold || !b.Reset.After(now) {
			continue
		}
		if b.Reset.After(until) {
			until = b.Reset
		}
		blocked = true
	}
	return until, blocked
}

// fresh reports whether the snapshot was written during the run that started
// at since. Stale snapshots predate the run and say nothing about how it
// ended — utilization may have dropped since they were written.
func (s rateLimitState) fresh(since time.Time) bool {
	return !s.WrittenAt.Before(since.Add(-writtenAtSlack))
}
