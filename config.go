package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBufferSecs = 60
	defaultPollSecs   = 5
	resumeMessage     = "Rate limits have reset - continuing where we left off."
)

type config struct {
	ClaudeBin       string
	ProjectsDir     string
	WarnFile        string
	DefaultArgs     []string
	Buffer          time.Duration
	SkipPermissions bool
	WatchInterval   time.Duration
	WatchTimeout    time.Duration
	WatchSettle     time.Duration
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
	Env             []string
	Now             func() time.Time
}

func loadConfig() (config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return config{}, fmt.Errorf("find home directory: %w", err)
	}

	self, _ := os.Executable()
	claudeBin := os.Getenv("CLAUDE_BIN")
	if claudeBin == "" {
		claudeBin, err = findClaudeBin(os.Getenv("PATH"), self)
		if err != nil {
			return config{}, err
		}
	}

	bufferSecs, err := intEnv("BUFFER_SECS", defaultBufferSecs)
	if err != nil {
		return config{}, err
	}

	claudeConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeConfigDir == "" {
		claudeConfigDir = filepath.Join(home, ".claude")
	}
	projectsDir := os.Getenv("PROJECTS_DIR")
	if projectsDir == "" {
		projectsDir = filepath.Join(claudeConfigDir, "projects")
	}

	watchSecs, err := intEnv("CLAUDE_SMART_RESUME_WATCH_SECS", defaultPollSecs)
	if err != nil {
		return config{}, err
	}
	if watchSecs == 0 {
		return config{}, errors.New("CLAUDE_SMART_RESUME_WATCH_SECS must be greater than zero")
	}

	settingsPath, explicitSettingsPath := settingsPath(home)
	defaultArgs, err := loadSettingsArgs(settingsPath, explicitSettingsPath)
	if err != nil {
		return config{}, fmt.Errorf("load settings %s: %w", settingsPath, err)
	}

	return config{
		ClaudeBin:       claudeBin,
		ProjectsDir:     projectsDir,
		WarnFile:        filepath.Join(claudeConfigDir, ".rl_warn"),
		DefaultArgs:     defaultArgs,
		Buffer:          time.Duration(bufferSecs) * time.Second,
		SkipPermissions: boolEnv("CLAUDE_SMART_RESUME_SKIP_PERMISSIONS"),
		WatchInterval:   time.Duration(watchSecs) * time.Second,
		WatchTimeout:    30 * time.Second,
		WatchSettle:     300 * time.Millisecond,
		Stdin:           os.Stdin,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		Env:             os.Environ(),
		Now:             time.Now,
	}, nil
}

type settingsFile struct {
	DefaultArgs []string `json:"defaultArgs"`
	ClaudeArgs  []string `json:"claudeArgs"`
}

func settingsPath(home string) (string, bool) {
	path := strings.TrimSpace(os.Getenv("CLAUDE_SMART_RESUME_CONFIG"))
	if path != "" {
		return path, true
	}
	return filepath.Join(home, ".config", "smarter_resume", "settings.json"), false
}

func loadSettingsArgs(path string, required bool) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var settings settingsFile
	if err := json.NewDecoder(f).Decode(&settings); err != nil {
		return nil, err
	}

	defaultArgs := settings.DefaultArgs
	if len(defaultArgs) == 0 {
		defaultArgs = settings.ClaudeArgs
	}
	return append([]string(nil), defaultArgs...), nil
}

func intEnv(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return n, nil
}

func boolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "t", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func findClaudeBin(pathEnv string, self string) (string, error) {
	if pathEnv == "" {
		return "", errors.New("CLAUDE_BIN is unset and PATH is empty")
	}

	var selfInfo os.FileInfo
	if self != "" {
		selfInfo, _ = os.Stat(self)
	}

	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		for _, name := range claudeExecutableNames() {
			candidate := filepath.Join(dir, name)
			info, err := os.Stat(candidate)
			if err != nil || info.IsDir() || !isExecutable(info) {
				continue
			}
			if selfInfo != nil && os.SameFile(info, selfInfo) {
				continue
			}
			return candidate, nil
		}
	}

	return "", errors.New("CLAUDE_BIN is unset and no real claude executable was found in PATH")
}

func claudeExecutableNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"claude.exe", "claude.cmd", "claude.bat", "claude"}
	}
	return []string{"claude"}
}

func isExecutable(info os.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

func initialArgs(defaultArgs []string, args []string, skipPermissions bool) []string {
	out := append([]string(nil), defaultArgs...)
	out = append(out, args...)
	if !skipPermissions || hasArg(out, "--dangerously-skip-permissions") {
		return dedupeNoValueFlags(out)
	}
	return dedupeNoValueFlags(append([]string{"--dangerously-skip-permissions"}, out...))
}

func hasArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}
