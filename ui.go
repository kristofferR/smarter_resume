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

// isTerminal reports whether w writes to a character device. The countdown's
// carriage-return rewrites turn into garbage in logs and pipes, so plain
// progress lines are used there instead.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

const nonTTYProgressInterval = 5 * time.Minute

func waitUntilWake(ctx context.Context, cfg config, wakeAt time.Time, sessionID string) bool {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	tty := isTerminal(cfg.Stderr)
	if !tty {
		if remaining := time.Until(wakeAt); remaining > 0 {
			fmt.Fprintf(cfg.Stderr, "  Waiting until %s (%s)\n", wakeAt.Format("15:04:05 MST"), formatRemaining(remaining))
		}
	}
	lastProgress := time.Now()

	for {
		remaining := time.Until(wakeAt)
		if remaining <= 0 {
			if tty {
				fmt.Fprint(cfg.Stderr, "\r\033[K")
			}
			return true
		}

		if tty {
			fmt.Fprintf(cfg.Stderr, "\r  Waiting until reset. Remaining: %s\033[K", formatRemaining(remaining))
		} else if time.Since(lastProgress) >= nonTTYProgressInterval {
			fmt.Fprintf(cfg.Stderr, "  Still waiting: %s remaining\n", formatRemaining(remaining))
			lastProgress = time.Now()
		}

		timer := time.NewTimer(minDuration(time.Second, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-interrupts:
			timer.Stop()
			if tty {
				fmt.Fprint(cfg.Stderr, "\r\033[K")
			}
			fmt.Fprintf(cfg.Stderr, "  Cancelled. Resume manually:\n  claude --resume %s\n\n", sessionID)
			return false
		case <-timer.C:
		}
	}
}

func formatRemaining(d time.Duration) string {
	return fmt.Sprintf("%d min %02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func minDuration(a time.Duration, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
