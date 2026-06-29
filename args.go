package main

import "strings"

type flagValueMode int

const (
	flagNone flagValueMode = iota
	flagOne
	flagOptionalOne
	flagMany
)

// Resume starts a new Claude process for the same session. Preserve startup
// context flags, but do not replay positional prompts or one-shot modes such as
// --print; the resume message below is the only prompt for the resumed turn.
//
// --session-id is deliberately absent: resume passes --resume <id>, and Claude
// rejects --session-id alongside --resume. cmux injects --session-id on the
// first launch, so it must be dropped (not preserved) when we resume.
var resumePassthroughFlags = makeResumePassthroughFlags()

func makeResumePassthroughFlags() map[string]flagValueMode {
	flags := map[string]flagValueMode{}
	add := func(mode flagValueMode, names ...string) {
		for _, name := range names {
			flags[name] = mode
		}
	}

	add(flagNone,
		"--allow-dangerously-skip-permissions",
		"--ax-screen-reader",
		"--bare",
		"--brief",
		"--chrome",
		"--dangerously-skip-permissions",
		"--disable-slash-commands",
		"--exclude-dynamic-system-prompt-sections",
		"--ide",
		"--no-chrome",
		"--safe-mode",
		"--strict-mcp-config",
		"--verbose",
	)
	add(flagOne,
		"--agent",
		"--agents",
		"--append-system-prompt",
		"--append-system-prompt-file",
		"--debug-file",
		"--effort",
		"--mcp-config",
		"--model",
		"--name",
		"-n",
		"--permission-mode",
		"--plugin-dir",
		"--plugin-url",
		"--setting-sources",
		"--settings",
		"--system-prompt",
		"--system-prompt-file",
		"--tools",
	)
	add(flagOptionalOne, "--debug")
	add(flagMany,
		"--add-dir",
		"--allowed-tools",
		"--allowedTools",
		"--betas",
		"--disallowed-tools",
		"--disallowedTools",
	)
	return flags
}

func resumeArgs(original []string, resumeID string) []string {
	args := extractResumePassthroughArgs(original)
	args = append(args, "--resume", resumeID, resumeMessage)
	return dedupeNoValueFlags(args)
}

func dedupeNoValueFlags(args []string) []string {
	out := make([]string, 0, len(args))
	seen := map[string]bool{}
	for _, arg := range args {
		name, hasInlineValue := splitFlagArg(arg)
		mode, ok := resumePassthroughFlags[name]
		if ok && !hasInlineValue && mode == flagNone {
			if seen[name] {
				continue
			}
			seen[name] = true
		}
		out = append(out, arg)
	}
	return out
}

func extractResumePassthroughArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}

		name, hasInlineValue := splitFlagArg(arg)
		mode, ok := resumePassthroughFlags[name]
		if !ok {
			continue
		}

		switch mode {
		case flagNone:
			if !hasInlineValue {
				out = append(out, arg)
			}
		case flagOne:
			if hasInlineValue {
				out = append(out, arg)
			} else if i+1 < len(args) {
				out = append(out, arg, args[i+1])
				i++
			} else {
				out = append(out, arg)
			}
		case flagOptionalOne:
			out = append(out, arg)
			if !hasInlineValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				out = append(out, args[i+1])
				i++
			}
		case flagMany:
			out = append(out, arg)
			if hasInlineValue {
				continue
			}
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				out = append(out, args[i+1])
				i++
			}
		}
	}
	return out
}

func splitFlagArg(arg string) (string, bool) {
	name, _, ok := strings.Cut(arg, "=")
	return name, ok
}
