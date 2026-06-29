package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"
)

func printRateLimitBanner(w io.Writer, sessionName string, resetAt time.Time, wakeAt time.Time, buffer time.Duration) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Smart Resume for Claude Code")
	fmt.Fprintln(w, "  Rate limit hit")
	fmt.Fprintf(w, "  Session  %q\n", sessionName)
	fmt.Fprintf(w, "  Resets   %s\n", resetAt.Format("15:04:05 MST  (2006-01-02)"))
	fmt.Fprintf(w, "  Waking   %s  (+%ds buffer)\n", wakeAt.Format("15:04:05 MST"), int(buffer.Seconds()))
	fmt.Fprintln(w, "  Press Ctrl-C to cancel")
	fmt.Fprintln(w)
}

func printResumeBanner(w io.Writer, sessionName string) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Resuming %q\n", sessionName)
	fmt.Fprintln(w)
}

func waitUntilWake(ctx context.Context, cfg config, wakeAt time.Time, sessionID string) bool {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	for {
		remaining := time.Until(wakeAt)
		if remaining <= 0 {
			fmt.Fprint(cfg.Stderr, "\r\033[K")
			return true
		}

		fmt.Fprintf(
			cfg.Stderr,
			"\r  Waiting until reset. Remaining: %d min %02ds\033[K",
			int(remaining.Minutes()),
			int(remaining.Seconds())%60,
		)

		timer := time.NewTimer(minDuration(time.Second, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-interrupts:
			timer.Stop()
			fmt.Fprintf(cfg.Stderr, "\r\033[K  Cancelled. Resume manually:\n  claude --resume %s\n\n", sessionID)
			return false
		case <-timer.C:
		}
	}
}

func minDuration(a time.Duration, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
