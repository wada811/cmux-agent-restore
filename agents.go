package main

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Runner runs an external command (cmux, ps, launchctl) once and returns its stdout.
// Tests replace it. When the command fails, it returns whatever output it got together with the error
// (ps -p exits 1 if a pid ended midway, but its output is still valid). The error carries stderr.
type Runner func(name string, args ...string) (string, error)

// Presence is whether an agent is running on a tab.
type Presence string

const (
	Present Presence = "present"
	Absent  Presence = "absent"
	Unknown Presence = "unknown"
)

// Foreground is whether only a shell is in the foreground of a tab's tty.
type Foreground string

const (
	FgShell   Foreground = "shell"
	FgBusy    Foreground = "busy"
	FgUnknown Foreground = "unknown"
)

// shells are the process names allowed in the foreground. If anything else is there, do not send.
var shells = []string{"zsh", "bash", "sh", "dash", "fish"}

// DefaultKinds are the restore_record kinds that get a cmux restore.
var DefaultKinds = []string{"claude", "codex"}

func basename(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// AgentProcess is the pid and kind of an agent found in ps.
type AgentProcess struct {
	PID  string
	Kind string
}

var psLineRe = regexp.MustCompile(`^(\d+)\s+(.*)$`)

// agentKind returns the kind of agent from the command part of one line of `ps -axo pid=,command=`,
// or "" if it is not an agent.
//
// An agent is a process whose executable name (basename) is claude or codex (the word after node,
// if it runs through node). Codex's resident processes (`codex app-server ...`, anything under
// app-server-daemon) and codex-code-mode-host are left out: they keep a stale CMUX_SURFACE_ID, so
// counting them would give wrong answers.
func agentKind(command string) string {
	tokens := strings.Fields(command)
	i := 0
	if len(tokens) > 0 && basename(tokens[0]) == "node" {
		i++
	}
	if i >= len(tokens) {
		return ""
	}
	exe := tokens[i]
	switch basename(exe) {
	case "claude":
		return "claude"
	case "codex":
		if strings.Contains(exe, "app-server-daemon") {
			return ""
		}
		if i+1 < len(tokens) && tokens[i+1] == "app-server" {
			return ""
		}
		return "codex"
	}
	return ""
}

// parseAgentProcesses picks the pid and kind of agents out of the output of `ps -axo pid=,command=`.
func parseAgentProcesses(psOutput string) []AgentProcess {
	var found []AgentProcess
	for _, line := range strings.Split(psOutput, "\n") {
		m := psLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if k := agentKind(m[2]); k != "" {
			found = append(found, AgentProcess{PID: m[1], Kind: k})
		}
	}
	return found
}

var surfaceIDRe = regexp.MustCompile(`CMUX_SURFACE_ID=([0-9A-Fa-f-]{36})`)

// parseSurfaceIDs collects the CMUX_SURFACE_ID UUIDs (upper case) from the output of `ps eww`.
func parseSurfaceIDs(psEwwOutput string) map[string]bool {
	found := map[string]bool{}
	for _, m := range surfaceIDRe.FindAllStringSubmatch(psEwwOutput, -1) {
		found[strings.ToUpper(m[1])] = true
	}
	return found
}

// surfacesWithAgent returns the set of surface UUIDs (upper case) found in the CMUX_SURFACE_ID
// environment variable of agents (claude or codex). It fails if the first ps fails.
func surfacesWithAgent(run Runner) (map[string]bool, error) {
	out, err := run("ps", "-axo", "pid=,command=")
	if err != nil {
		// The list may be incomplete, so fail even if there is output. Wrongly deciding that no agent
		// is there would send the command to a tab where an agent is running
		return nil, err
	}
	procs := parseAgentProcesses(out)
	if len(procs) == 0 {
		return map[string]bool{}, nil
	}
	pids := make([]string, len(procs))
	for i, p := range procs {
		pids[i] = p.PID
	}
	// exit 1 when a pid ended midway, but the output is still valid, so ignore the error
	envOut, _ := run("ps", "eww", "-p", strings.Join(pids, ","), "-o", "command=")
	return parseSurfaceIDs(envOut), nil
}

// checkAgentOnSurface decides whether an agent is on a surface.
// It says Absent only when two looks, with a pause between them, both saw nothing.
// When it cannot decide (no surfaceID, ps failed) it returns Unknown, and the caller treats that as
// "an agent is there".
func checkAgentOnSurface(surfaceID string, probe func() (map[string]bool, error), sleep func(time.Duration), interval time.Duration) Presence {
	if surfaceID == "" {
		return Unknown
	}
	target := strings.ToUpper(surfaceID)
	set, err := probe()
	if err != nil {
		return Unknown
	}
	if set[target] {
		return Present
	}
	sleep(interval)
	set, err = probe()
	if err != nil {
		return Unknown
	}
	if set[target] {
		return Present
	}
	return Absent
}

// selectTargets picks the tabs to send a restore to.
// A target is a tab whose restore_record kind is in kinds and where no agent (of any kind) is running.
// Tabs for which kindOf returns "" (no record, or unreadable) and tabs with an unknown kind are not targets.
func selectTargets(tabs []Tab, withAgent map[string]bool, kindOf func(Tab) string, kinds []string) []Tab {
	var out []Tab
	for _, t := range tabs {
		if withAgent[strings.ToUpper(t.UUID)] {
			continue
		}
		k := kindOf(t)
		if k != "" && slices.Contains(kinds, k) {
			out = append(out, t)
		}
	}
	return out
}

var psTtyLineRe = regexp.MustCompile(`^\d+\s+(\S+)\s+(.+)$`)

// classifyForeground decides from the output of `ps -t <tty> -o pid=,stat=,comm=` whether only a shell
// is in the foreground of that tty (stat has a +). If no foreground process is visible, FgUnknown.
// It does not read environment variables: cmux 0.65.2 starts the shell as a child of a root-owned
// login, so the shell's environment cannot be read.
func classifyForeground(psTtyOutput string) Foreground {
	foreground := 0
	for _, line := range strings.Split(psTtyOutput, "\n") {
		m := psTtyLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || !strings.Contains(m[1], "+") {
			continue
		}
		foreground++
		name := strings.TrimPrefix(basename(strings.TrimSpace(m[2])), "-")
		if !slices.Contains(shells, name) {
			return FgBusy
		}
	}
	if foreground > 0 {
		return FgShell
	}
	return FgUnknown
}

// ttyHasOtherSurface reports whether the output of `ps eww -t <tty> -o command=` has a process with
// another surface's CMUX_SURFACE_ID. If so, the tty in `cmux tree` is stale and points at another tab.
// Login and shell lines carry no CMUX_SURFACE_ID, so this alone is not enough (see sharedTTYs).
func ttyHasOtherSurface(psEwwOutput, surfaceID string) bool {
	own := strings.ToUpper(surfaceID)
	for _, m := range regexp.MustCompile(`CMUX_SURFACE_ID=([0-9A-Fa-f-]+)`).FindAllStringSubmatch(psEwwOutput, -1) {
		if strings.ToUpper(m[1]) != own {
			return true
		}
	}
	return false
}

var ttyNameRe = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// foregroundOnTty checks with a real ps whether only a shell is in the foreground of a tty (the
// surface.tty of `cmux tree`, for example ttys117). It returns FgUnknown when there is no tty, ps
// fails, or the tty has a process from another tab.
func foregroundOnTty(run Runner, tty, surfaceID string) Foreground {
	if tty == "" || !ttyNameRe.MatchString(tty) {
		return FgUnknown
	}
	eww, err := run("ps", "eww", "-t", tty, "-o", "command=")
	if err != nil {
		return FgUnknown
	}
	if ttyHasOtherSurface(eww, surfaceID) {
		return FgUnknown
	}
	out, err := run("ps", "-t", tty, "-o", "pid=,stat=,comm=")
	if err != nil {
		return FgUnknown
	}
	return classifyForeground(out)
}

var errNoCmux = errors.New("cmux not found")

// sharedTTYs returns the ttys that two or more tabs claim in the tree.
// A tab's tty in `cmux tree` can be stale and point at another tab's terminal. Login and shell
// processes do not carry CMUX_SURFACE_ID, so ps alone cannot tell; tabs that share a tty cannot be
// told apart either, and neither is safe to type into.
func sharedTTYs(tabs []Tab) map[string]bool {
	count := map[string]int{}
	for _, t := range tabs {
		if t.TTY != "" {
			count[t.TTY]++
		}
	}
	shared := map[string]bool{}
	for tty, n := range count {
		if n > 1 {
			shared[tty] = true
		}
	}
	return shared
}
