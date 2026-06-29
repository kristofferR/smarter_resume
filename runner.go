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
	firstArgs := initialArgs(cfg.DefaultArgs, args, cfg.SkipPermissions)

	for {
		preRunFile, preRunLines := snapshotLatestSession(cfg.ProjectsDir, cwd)

		runArgs := firstArgs
		if resumeID != "" {
			runArgs = resumeArgs(firstArgs, resumeID)
		}

		runStarted := time.Now()
		exitCode, startErr := runClaude(ctx, cfg, runArgs, cwd, runStarted, preRunFile, preRunLines)
		lastExit = exitCode
		if startErr != nil {
			fmt.Fprintf(cfg.Stderr, "smarter_resume: run claude: %v\n", startErr)
			return exitCode
		}

		sessionFile, ok, err := findRunSession(cfg.ProjectsDir, cwd, runStarted, preRunFile, preRunLines)
		if err != nil {
			fmt.Fprintf(cfg.Stderr, "smarter_resume: find session: %v\n", err)
			return lastExit
		}
		if !ok {
			return lastExit
		}

		startLine := 1
		if sessionFile == preRunFile {
			startLine = preRunLines + 1
		}

		resetAt, ok, err := resetAfterRun(cfg, sessionFile, startLine)
		if err != nil {
			fmt.Fprintf(cfg.Stderr, "smarter_resume: parse reset time: %v\n", err)
			return lastExit
		}
		if !ok {
			return lastExit
		}

		sessionID := sessionIDFromPath(sessionFile)
		sessionName, err := ensureSessionName(cfg, sessionFile, sessionID, cwd)
		if err != nil {
			fmt.Fprintf(cfg.Stderr, "smarter_resume: name session: %v\n", err)
			return lastExit
		}

		wakeAt := resetAt.Add(cfg.Buffer)
		printRateLimitBanner(cfg.Stderr, sessionName, resetAt, wakeAt, cfg.Buffer)
		if !waitUntilWake(ctx, cfg, wakeAt, sessionID) {
			return cancelledWaitExitCode(lastExit)
		}
		printResumeBanner(cfg.Stderr, sessionName)
		resumeID = sessionID
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

func resetAfterRun(cfg config, sessionFile string, startLine int) (time.Time, bool, error) {
	if reset, ok, err := resetFromWarnFile(cfg.WarnFile); err != nil || ok {
		return reset, ok, err
	}

	info, ok, err := findResetInfo(sessionFile, startLine)
	if err != nil || !ok {
		return time.Time{}, ok, err
	}

	reset, err := parseResetTime(info.Text, info.TZ, cfg.Now())
	if err != nil {
		return time.Time{}, false, err
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

func runClaude(ctx context.Context, cfg config, args []string, cwd string, runStarted time.Time, preRunFile string, preRunLines int) (int, error) {
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
			return 127, err
		}
		return 126, err
	}

	watchCtx, stopWatcher := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		watchForRateLimit(watchCtx, cfg, cwd, cmd.Process, runStarted, preRunFile, preRunLines)
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

	return processExitCode(cmd.ProcessState, err), nil
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

func watchForRateLimit(ctx context.Context, cfg config, cwd string, process *os.Process, runStarted time.Time, preRunFile string, preRunLines int) {
	sessionFile, ok := waitForSessionFile(ctx, cfg, cwd, runStarted, preRunFile, preRunLines)
	if !ok {
		return
	}

	baseline := 0
	if sessionFile == preRunFile {
		baseline = preRunLines
	}
	if signalIfRateLimited(ctx, cfg, process, sessionFile, baseline+1) {
		return
	}
	current, err := countLines(sessionFile)
	if err != nil {
		return
	}
	if current > baseline {
		baseline = current
	}

	ticker := time.NewTicker(cfg.WatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := countLines(sessionFile)
			if err != nil {
				return
			}
			if current <= baseline {
				continue
			}
			if signalIfRateLimited(ctx, cfg, process, sessionFile, baseline+1) {
				return
			}
			baseline = current
		}
	}
}

func signalIfRateLimited(ctx context.Context, cfg config, process *os.Process, sessionFile string, startLine int) bool {
	if _, ok, err := findResetInfo(sessionFile, startLine); err != nil || !ok {
		return false
	}
	if cfg.WatchSettle > 0 {
		timer := time.NewTimer(cfg.WatchSettle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return true
		case <-timer.C:
		}
	}
	_ = process.Signal(os.Interrupt)
	return true
}

func waitForSessionFile(ctx context.Context, cfg config, cwd string, runStarted time.Time, preRunFile string, preRunLines int) (string, bool) {
	deadline := time.NewTimer(cfg.WatchTimeout)
	defer deadline.Stop()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		if sessionFile, ok, err := findRunSession(cfg.ProjectsDir, cwd, runStarted, preRunFile, preRunLines); err == nil && ok {
			return sessionFile, true
		}

		select {
		case <-ctx.Done():
			return "", false
		case <-deadline.C:
			return "", false
		case <-ticker.C:
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
