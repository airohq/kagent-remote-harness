package supervisor

import (
	"fmt"
	"strings"
)

// The adapter emits the permission setting itself, including the different
// spelling required by codex exec resume. Keep other native arguments intact.
func nativeArgs(harness, mode string, args []string) (string, []string, error) {
	var extra []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, assigned := strings.Cut(arg, "=")
		if harness == "codex" && strings.HasPrefix(name, "-s") && len(name) > 2 {
			name, value, assigned = "-s", arg[2:], true
		}
		if (harness == "codex" && (name == "--sandbox" || name == "-s")) ||
			(harness == "claude" && name == "--permission-mode") {
			if !assigned && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value = args[i]
			}
			if value == "" {
				return "", nil, fmt.Errorf("%s needs a permission mode", name)
			}
			mode = value
			continue
		}
		if managedArg(harness, name) {
			return "", nil, fmt.Errorf("%s is managed by the runner for %s turns", name, harness)
		}
		extra = append(extra, arg)
	}
	return mode, extra, nil
}

func managedArg(harness, name string) bool {
	switch name {
	case "--", "--help", "-h", "--version", "-V":
		return true
	}
	switch harness {
	case "claude":
		switch name {
		case "-p", "--print", "--input-format", "--output-format", "--verbose", "--include-partial-messages", "-c", "--continue", "-r", "--resume", "--session-id", "--fork-session", "--no-session-persistence":
			return true
		}
	case "codex":
		switch name {
		case "--json", "--experimental-json", "--skip-git-repo-check", "--cd", "--ephemeral":
			return true
		}
		return strings.HasPrefix(name, "-C")
	}
	return false
}
