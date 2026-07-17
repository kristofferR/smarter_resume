package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func run(ctx context.Context, cfg config, args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(cfg.Stderr, "smarter_resume: find working directory: %v\n", err)
		return 2
	}

	resumeID := ""
	lastExit := 0
	extraBuffer := time.Duration(0)
	var lastResumeAt time.Time
	firstArgs := initialArgs(cfg.DefaultArgs, args, cfg.SkipPermissions)

	for {
		preRunFile, preRunLines := snapshotLatestSession(cfg.ProjectsDir, cwd)

		runArgs := firstArgs
		if resumeID != "" {
			runArgs = resumeArgs(firstArgs, resumeID)
		}

		runStarted := time.Now()
		exitCode, watched, startErr := runClaude(ctx, cfg, runArgs, cwd, runStarted, preRunFile, preRunLines)
		lastExit = exitCode
		if startErr != nil {
			fmt.Fprintf(cfg.Stderr, "smarter_resume: run claude: %v\n", startErr)
			return exitCode
		}

		sessionFile := watched.sessionFile
		if sessionFile == "" {
			var ok bool
			var err error
			sessionFile, ok, err = findRunSession(cfg.ProjectsDir, cwd, runStarted, preRunFile, preRunLines)
			if err != nil {
				fmt.Fprintf(cfg.Stderr, "smarter_resume: find session: %v\n", err)
				return lastExit
			}
			if !ok {
				return lastExit
			}
		}

		var resetAt time.Time
		if !watched.limitResume {
			startLine := 1
			if sessionFile == preRunFile {
				startLine = preRunLines + 1
			}

			var ok bool
			var err error
			resetAt, ok, err = resetAfterRun(cfg, sessionFile, startLine, runStarted)
			if err != nil {
				fmt.Fprintf(cfg.Stderr, "smarter_resume: parse reset time: %v\n", err)
				return lastExit
			}
			if !ok {
				return lastExit
			}
		}

		// A limit detection shortly after a resume means the reset estimate
		// was early or the resumed turn immediately re-hit the limit — back
		// off instead of relaunching in a tight loop.
		if !lastResumeAt.IsZero() && cfg.Now().Sub(lastResumeAt) < cfg.RelaunchGuardWindow {
			extraBuffer = minDuration(extraBuffer*2+cfg.Buffer, maxExtraBuffer)
		} else {
			extraBuffer = 0
		}

		sessionID := sessionIDFromPath(sessionFile)
		sessionName, err := ensureSessionName(cfg, sessionFile, sessionID, cwd)
		if err != nil {
			fmt.Fprintf(cfg.Stderr, "smarter_resume: name session: %v\n", err)
			return lastExit
		}

		if watched.limitResume {
			// The watcher already waited the limit out while claude (and its
			// background processes) kept running, verified the limits lifted,
			// and only then ended the run — resume right away, honoring only
			// the tight-relaunch back-off.
			fmt.Fprintln(cfg.Stderr, "\n  Rate limit lifted — restarting to continue")
			if extraBuffer > 0 && !waitUntilLimitsLift(ctx, cfg, cfg.Now().Add(extraBuffer), sessionID, runStarted, extraBuffer) {
				return cancelledWaitExitCode(lastExit)
			}
		} else {
			buffer := cfg.Buffer + extraBuffer
			wakeAt := resetAt.Add(buffer)
			printRateLimitBanner(cfg.Stderr, sessionName, resetAt, wakeAt, buffer)
			if !waitUntilLimitsLift(ctx, cfg, wakeAt, sessionID, runStarted, buffer) {
				return cancelledWaitExitCode(lastExit)
			}
		}
		printResumeBanner(cfg.Stderr, sessionName)
		resumeID = sessionID
		lastResumeAt = cfg.Now()
	}
}

// maxExtraBuffer caps the back-off added on top of the configured buffer when
// resumes re-hit the limit in quick succession.
const maxExtraBuffer = 15 * time.Minute

// waitUntilLimitsLift waits until wakeAt and then verifies against the
// rate-limit snapshot that the limits actually lifted, extending the wait when
// they did not. The snapshot's absolute reset epochs stay valid while claude
// is not running, so a wrong or early transcript-parsed reset self-corrects
// here instead of triggering a premature resume.
func waitUntilLimitsLift(ctx context.Context, cfg config, wakeAt time.Time, sessionID string, runStarted time.Time, buffer time.Duration) bool {
	for {
		if !waitUntilWake(ctx, cfg, wakeAt, sessionID) {
			return false
		}
		state, ok, err := loadRateLimitState(cfg.StateFile, cfg.WarnFile)
		if err != nil || !ok || !state.fresh(runStarted) {
			return true
		}
		until, blocked := state.blockedUntil(cfg.Now())
		if !blocked {
			return true
		}
		wakeAt = until.Add(buffer)
		fmt.Fprintf(cfg.Stderr, "  Limits still exhausted; waiting until %s\n", wakeAt.Format("15:04:05 MST"))
	}
}

func snapshotLatestSession(projectsDir string, cwd string) (string, int) {
	path, ok, err := findLatestSession(projectsDir, cwd)
	if err != nil || !ok {
		return "", 0
	}
	lines, err := countLines(path)
	if err != nil {
		return path, 0
	}
	return path, lines
}

// resetAfterRun decides whether the run that just ended hit a usage limit and
// when it lifts. The statusline snapshot is the primary signal: it carries
// exact utilization and reset epochs, so a limit is only declared when a
// bucket actually reached 100%. The transcript text is the fallback — the
// snapshot can lag the final rate-limited request by one render, or be
// missing entirely on older statusline versions.
func resetAfterRun(cfg config, sessionFile string, startLine int, runStarted time.Time) (time.Time, bool, error) {
	if state, ok, err := loadRateLimitState(cfg.StateFile, cfg.WarnFile); err == nil && ok && state.fresh(runStarted) {
		if until, blocked := state.blockedUntil(cfg.Now()); blocked {
			return until, true, nil
		}
	}

	info, ok, err := findResetInfo(sessionFile, startLine)
	if err != nil || !ok {
		return time.Time{}, ok, err
	}

	// Anchor ambiguous times ("resets 3pm") to the moment the notice was
	// written, not the moment claude exited — the session may have continued
	// long past the notice.
	ref := info.When
	if ref.IsZero() {
		ref = cfg.Now()
	}
	reset, err := parseResetTime(info.Text, info.TZ, ref)
	if err != nil {
		return time.Time{}, false, err
	}

	now := cfg.Now()
	if !reset.After(now) {
		if now.Sub(reset) <= resetGraceWindow {
			// The reset passed while claude was shutting down — it already
			// lifted, resume immediately.
			return now, true, nil
		}
		// The limit lifted long before claude exited, so the session was
		// waited out or continued — this exit was not limit-caused.
		return time.Time{}, false, nil
	}
	return reset, true, nil
}

func findRunSession(projectsDir string, cwd string, runStarted time.Time, preRunFile string, preRunLines int) (string, bool, error) {
	sessionFile, ok, err := findLatestSessionAfter(projectsDir, cwd, runStarted)
	if err != nil || ok {
		return sessionFile, ok, err
	}

	if preRunFile == "" {
		return "", false, nil
	}
	lines, err := countLines(preRunFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if lines > preRunLines {
		return preRunFile, true, nil
	}
	return "", false, nil
}

func cancelledWaitExitCode(lastExit int) int {
	if lastExit != 0 {
		return lastExit
	}
	return 130
}

func ensureSessionName(cfg config, sessionFile string, sessionID string, cwd string) (string, error) {
	name, found, err := latestSessionTitle(sessionFile)
	if err != nil {
		return "", err
	}
	if found && name != "" {
		return name, nil
	}

	name = generateSessionName(cfg.Now(), cwd)
	if err := appendSessionTitle(sessionFile, sessionID, name); err != nil {
		return "", err
	}
	return name, nil
}

func sessionIDFromPath(path string) string {
	return filepath.Base(path[:len(path)-len(filepath.Ext(path))])
}

// watchResult reports what the in-run watcher concluded. limitResume means the
// watcher hit a usage limit, waited it out while claude kept running, verified
// the limits lifted, and then ended the run — the caller should resume the
// session immediately.
type watchResult struct {
	limitResume bool
	sessionFile string
}

func runClaude(ctx context.Context, cfg config, args []string, cwd string, runStarted time.Time, preRunFile string, preRunLines int) (int, watchResult, error) {
	_ = os.Remove(cfg.WarnFile)

	bin, env := resolveLaunch(cfg, args)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = cfg.Stdin
	cmd.Stdout = cfg.Stdout
	cmd.Stderr = cfg.Stderr
	cmd.Env = env
	cmd.Dir = cwd

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return 127, watchResult{}, err
		}
		return 126, watchResult{}, err
	}

	watchCtx, stopWatcher := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	var watched watchResult
	go func() {
		defer close(watcherDone)
		watched = watchForRateLimit(watchCtx, cfg, cwd, cmd.Process, runStarted, preRunFile, preRunLines)
	}()

	forwardInterrupts := shouldForwardInterrupt(cmd.Process)
	interrupts := make(chan os.Signal, 4)
	signal.Notify(interrupts, os.Interrupt)
	stopSignals := make(chan struct{})
	signalDone := make(chan struct{})
	go func() {
		defer close(signalDone)
		for {
			select {
			case <-interrupts:
				if forwardInterrupts {
					_ = cmd.Process.Signal(os.Interrupt)
				}
			case <-stopSignals:
				return
			}
		}
	}()

	err := cmd.Wait()
	stopWatcher()
	<-watcherDone
	signal.Stop(interrupts)
	close(stopSignals)
	<-signalDone

	return processExitCode(cmd.ProcessState, err), watched, nil
}

// resolveLaunch decides which binary to exec and with what environment so that
// cmux's claude hooks fire exactly once and the launch never recurses.
//
//   - If cmux already injected its hooks (it launched smarter_resume as its
//     claude binary, so the hook --settings are already in args), run the real
//     claude directly and pass those args straight through.
//   - Otherwise, inside a cmux surface with a shim available (e.g. launched via
//     a `claude`/`cc` alias), route through cmux's shim so it injects hooks —
//     pinning CMUX_CUSTOM_CLAUDE_PATH to the real claude so the shim resolves
//     back to claude instead of recursing into smarter_resume.
//   - With no cmux shim (or hooks already present), run the real claude.
func resolveLaunch(cfg config, args []string) (string, []string) {
	if cfg.CmuxShim == "" || argsHaveCmuxHooks(args) {
		return cfg.ClaudeBin, cfg.Env
	}
	env := setEnvVar(cfg.Env, "CMUX_CUSTOM_CLAUDE_PATH", cfg.ClaudeBin)
	return cfg.CmuxShim, env
}

// argsHaveCmuxHooks reports whether args already carry cmux's injected hook
// configuration, signalling that cmux wrapped this process and we must not wrap
// it again.
func argsHaveCmuxHooks(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var value string
		switch {
		case strings.HasPrefix(arg, "--settings="):
			value = strings.TrimPrefix(arg, "--settings=")
		case arg == "--settings" && i+1 < len(args):
			value = args[i+1]
			i++
		default:
			continue
		}
		if strings.Contains(value, "CMUX_CLAUDE_HOOK") ||
			strings.Contains(value, "hooks claude ") ||
			strings.Contains(value, "hooks feed") {
			return true
		}
	}
	return false
}

// setEnvVar returns a copy of env with key set to value, replacing any existing
// definition.
func setEnvVar(env []string, key string, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, prefix+value)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}

func shouldForwardInterrupt(process *os.Process) bool {
	if process == nil {
		return false
	}
	childPGID, err := syscall.Getpgid(process.Pid)
	if err != nil {
		return true
	}
	return childPGID != syscall.Getpgrp()
}

// watchForRateLimit watches the running claude for a hit usage limit. It does
// NOT end the run at detection time — killing claude also kills its background
// shells, which may be mid-task. Instead the limit is waited out while claude
// keeps running; only once the limits have verifiably lifted (and the session
// is still stalled) is claude restarted so the resume prompt can continue the
// work. Each tick it polls the rate-limit snapshot (which needs no session
// file), keeps trying to resolve this run's session file, and scans new
// transcript lines for a genuine limit notice.
func watchForRateLimit(ctx context.Context, cfg config, cwd string, process *os.Process, runStarted time.Time, preRunFile string, preRunLines int) watchResult {
	ticker := time.NewTicker(cfg.WatchInterval)
	defer ticker.Stop()

	sessionFile := ""
	baseline := 0

	for {
		var resetAt time.Time
		detected := false

		if until, blocked := stateBlockedUntil(cfg, runStarted); blocked {
			resetAt = until
			detected = true
		}

		if sessionFile == "" {
			if file, ok, err := findRunSession(cfg.ProjectsDir, cwd, runStarted, preRunFile, preRunLines); err == nil && ok {
				sessionFile = file
				if sessionFile == preRunFile {
					baseline = preRunLines
				}
			}
		}

		if sessionFile != "" {
			current, err := countLines(sessionFile)
			if err == nil && current > baseline {
				if info, ok, err := findResetInfo(sessionFile, baseline+1); err == nil && ok && !detected {
					if reset, ok := noticeResetTime(cfg, info); ok {
						resetAt = reset
						detected = true
					}
				}
				baseline = current
			}
		}

		if detected {
			if waitOutLimit(ctx, cfg, resetAt, runStarted, sessionFile) {
				terminateClaude(ctx, cfg, process)
				return watchResult{limitResume: true, sessionFile: sessionFile}
			}
			if ctx.Err() != nil {
				return watchResult{sessionFile: sessionFile}
			}
			// The session produced output after the reset — it already
			// continued on its own (or the detection was wrong). Keep
			// watching instead of restarting it out from under the user.
		}

		select {
		case <-ctx.Done():
			return watchResult{sessionFile: sessionFile}
		case <-ticker.C:
		}
	}
}

// stateBlockedUntil reports whether the rate-limit snapshot says a bucket is
// exhausted for the current run and until when.
func stateBlockedUntil(cfg config, runStarted time.Time) (time.Time, bool) {
	state, ok, err := loadRateLimitState(cfg.StateFile, cfg.WarnFile)
	if err != nil || !ok || !state.fresh(runStarted) {
		return time.Time{}, false
	}
	return state.blockedUntil(cfg.Now())
}

// noticeResetTime turns a transcript limit notice into an absolute reset time,
// anchored to the record's own timestamp when it has one.
func noticeResetTime(cfg config, info resetInfo) (time.Time, bool) {
	ref := info.When
	if ref.IsZero() {
		ref = cfg.Now()
	}
	reset, err := parseResetTime(info.Text, info.TZ, ref)
	if err != nil {
		return time.Time{}, false
	}
	return reset, true
}

// waitOutLimit sleeps until the detected limit has actually lifted, extending
// the wait whenever the snapshot still reports a blocking bucket (its absolute
// reset epochs stay valid throughout). Returns true when the session should be
// restarted: limits lifted with no transcript activity since, i.e. the session
// is genuinely stalled. Returns false when ctx is cancelled (claude exited on
// its own) or when the transcript shows post-reset activity.
func waitOutLimit(ctx context.Context, cfg config, resetAt time.Time, runStarted time.Time, sessionFile string) bool {
	for {
		if !sleepUntil(ctx, cfg, resetAt.Add(cfg.Buffer)) {
			return false
		}
		state, ok, err := loadRateLimitState(cfg.StateFile, cfg.WarnFile)
		if err != nil || !ok || !state.fresh(runStarted) {
			break
		}
		if until, blocked := state.blockedUntil(cfg.Now()); blocked {
			resetAt = until
			continue
		}
		break
	}

	if sessionFile != "" {
		if info, err := os.Stat(sessionFile); err == nil && info.ModTime().After(resetAt) {
			return false
		}
	}
	return true
}

// sleepUntil waits until wakeAt in short slices so wall-clock jumps (system
// sleep) cannot oversleep by more than a tick. It stays silent: claude owns
// the terminal while this runs.
func sleepUntil(ctx context.Context, cfg config, wakeAt time.Time) bool {
	for {
		remaining := wakeAt.Sub(cfg.Now())
		if remaining <= 0 {
			return true
		}
		timer := time.NewTimer(minDuration(time.Second, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// terminateClaude escalates until the child exits. Interactive claude treats a
// single Ctrl-C as "cancel the current input" and only quits on a second one
// shortly after, so a lone SIGINT would leave the wrapper blocked in cmd.Wait
// forever. ctx is cancelled as soon as cmd.Wait returns, which aborts the
// escalation at the first sign of exit.
func terminateClaude(ctx context.Context, cfg config, process *os.Process) {
	steps := []struct {
		sig  os.Signal
		wait time.Duration
	}{
		{os.Interrupt, cfg.InterruptRepeat},
		{os.Interrupt, cfg.InterruptGrace},
		{syscall.SIGTERM, cfg.TermGrace},
		{os.Kill, 0},
	}
	for _, step := range steps {
		if ctx.Err() != nil {
			return
		}
		_ = process.Signal(step.sig)
		if step.wait <= 0 {
			continue
		}
		timer := time.NewTimer(step.wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func processExitCode(state *os.ProcessState, err error) int {
	if err == nil {
		return 0
	}
	if state != nil {
		if status, ok := state.Sys().(syscall.WaitStatus); ok {
			if status.Exited() {
				return status.ExitStatus()
			}
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
		}
		if code := state.ExitCode(); code >= 0 {
			return code
		}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		if code := exitErr.ProcessState.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}
