package main

import (
	"fmt"
	"strings"
	"time"
)

// Defaults. Flags can override them.
const (
	defaultBatch         = 3
	defaultBatchInterval = 20 * time.Second
	defaultMaxAttempts   = 3
	presenceInterval     = 1500 * time.Millisecond
	statusKey            = "agent_restore"
	// unknownLimit is how many runs in a row a tab may stay unverifiable before it is shown in the sidebar.
	unknownLimit = 3
)

// Options are the flags of run.
type Options struct {
	DryRun        bool
	Batch         int
	BatchInterval time.Duration
	MaxAttempts   int
	Kinds         []string
}

func defaultOptions() Options {
	return Options{Batch: defaultBatch, BatchInterval: defaultBatchInterval, MaxAttempts: defaultMaxAttempts, Kinds: DefaultKinds}
}

// Env is how run reaches the outside. Tests replace it.
type Env struct {
	Run              Runner
	Cmux             string
	Sleep            func(time.Duration)
	Logf             func(format string, args ...any)
	StatePath        string
	Now              func() time.Time
	PresenceInterval time.Duration
}

// listTerminalTabs returns the refs of all workspaces and the list of terminal tabs. ok is false if cmux does not answer.
func (e *Env) listTerminalTabs() (workspaceRefs []string, tabs []Tab, ok bool) {
	out, err := e.Run(e.Cmux, "tree", "--all", "--id-format", "both", "--json")
	if err != nil {
		return nil, nil, false
	}
	refs, tabs, err := parseTree(out)
	if err != nil {
		return nil, nil, false
	}
	return refs, tabs, true
}

// recheckTab reads the tree again and returns the tab as it is now. The reason is non-empty when the
// tab must not be sent to: the tree cannot be read, the tab is gone, its tty differs from the first
// read, or another tab claims the same tty. unverifiable is true for the last two: the tab is there
// but we cannot tell which terminal it owns.
func (e *Env) recheckTab(t Tab) (fresh Tab, reason string, unverifiable bool) {
	_, tabs, ok := e.listTerminalTabs()
	if !ok {
		return Tab{}, "cannot re-read the tab list", false
	}
	var cur *Tab
	for i := range tabs {
		if tabs[i].UUID == t.UUID {
			cur = &tabs[i]
			break
		}
	}
	switch {
	case cur == nil:
		return Tab{}, "tab no longer exists", false
	case cur.TTY != t.TTY:
		return Tab{}, "tty changed since the first read (" + t.TTY + " -> " + cur.TTY + ")", true
	case sharedTTYs(tabs)[cur.TTY]:
		return Tab{}, "other tabs share a tty with this tab (" + cur.TTY + ")", true
	}
	return *cur, "", false
}

// warn logs a failure as WARN.
func (e *Env) warn(what string, err error) {
	msg := err.Error()
	if i := strings.Index(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	e.Logf("WARN %s failed: %s", what, msg)
}

// Restore looks over the tabs once and sends cmux restore to tabs that were not restored.
//   - Targets: terminal tabs that have a restore record (restore_record.checkpoint_id) whose kind is in
//     Kinds and where no agent process (of any kind) runs. Tabs with an unknown or unreadable kind get nothing.
//   - Whether an agent is there is not decided by tty (the tty in `cmux tree` can be stale). It reads
//     CMUX_SURFACE_ID from each agent pid and matches it with the surface UUIDs of the tree.
//   - The target list is built once, and sending takes minutes, during which an agent may start. So just
//     before sending to each tab, checkAgentOnSurface looks again, and we send only if it is confirmed absent.
//     The tab's foreground must also be only a shell (foregroundOnTty), and the tree is re-read to confirm
//     the tab still exists with the same tty.
//     Tabs with an agent, tabs we cannot decide on, and tabs whose foreground is not a shell are skipped
//     for this run and do not count as an attempt.
//   - Restoring everything at once has hit pool_saturated, so send Batch tabs at a time, BatchInterval apart.
//   - Kinds of failure are not told apart. A tab that has not come back after MaxAttempts sends is not sent
//     to any more (to stop typing the same command into a shell). It is only shown in the sidebar of its
//     workspace as agent_restore. Workspace refs change when cmux restarts, so where it was shown is not
//     remembered; every run decides again, for all workspaces, whether to show or clear it.
//   - If cmux does not answer, do nothing.
func Restore(env *Env, opt Options) error {
	workspaceRefs, tabs, ok := env.listTerminalTabs()
	if !ok {
		return nil
	}

	withAgent, err := surfacesWithAgent(env.Run)
	if err != nil {
		return fmt.Errorf("ps failed: %w", err)
	}
	state := loadState(env.StatePath)

	// Targets are the tabs with no agent that have a restore record of a targeted kind.
	// Records are not read for tabs where an agent is running.
	var candidates []Tab
	for _, t := range tabs {
		if !withAgent[strings.ToUpper(t.UUID)] {
			candidates = append(candidates, t)
		}
	}
	kindList, readErrs := restoreKinds(env.Run, env.Cmux, candidates)
	kinds := map[string]string{}
	unreadable := map[string]bool{}
	for i, t := range candidates {
		kinds[t.UUID] = kindList[i]
		if readErrs[i] != nil {
			unreadable[t.UUID] = true
			env.warn("could not read the restore record of "+t.Ref, readErrs[i])
		}
	}
	targets := selectTargets(tabs, withAgent, func(t Tab) string { return kinds[t.UUID] }, opt.Kinds)
	targetUUIDs := map[string]bool{}
	for _, t := range targets {
		targetUUIDs[t.UUID] = true
	}

	// Drop tabs that came back, were closed, or lost their record. A tab whose record could not be read
	// is kept: we do not know that it is gone, and dropping its count would restart an exhausted tab
	for uuid := range state.Attempts {
		if !targetUUIDs[uuid] && !unreadable[uuid] {
			delete(state.Attempts, uuid)
		}
	}

	var stuck, toSend []Tab
	sendable := map[string]bool{}
	for _, t := range targets {
		if state.Attempts[t.UUID] >= opt.MaxAttempts {
			stuck = append(stuck, t)
		} else {
			toSend = append(toSend, t)
			sendable[t.UUID] = true
		}
	}
	// The streak only means something for tabs we still try to send to
	for uuid := range state.UnknownStreak {
		if !sendable[uuid] && !unreadable[uuid] {
			delete(state.UnknownStreak, uuid)
		}
	}
	env.Logf("targets %d (to send %d, given up %d)", len(targets), len(toSend), len(stuck))
	for _, t := range targets {
		env.Logf("  %s %s %s attempts %d/%d", t.Ref, t.UUID, kinds[t.UUID], state.Attempts[t.UUID], opt.MaxAttempts)
	}

	save := func() {
		if opt.DryRun {
			return
		}
		if err := saveState(env.StatePath, state); err != nil {
			env.warn("saving state", err)
		}
	}

	probe := func() (map[string]bool, error) { return surfacesWithAgent(env.Run) }
	sentInPrevBatch := false
	for i := 0; i < len(toSend); i += opt.Batch {
		// Do not wait after a batch that sent nothing (agents had already started). The pause that avoids pool_saturated is only needed after sending
		if sentInPrevBatch && !opt.DryRun {
			env.Sleep(opt.BatchInterval)
		}
		sentInPrevBatch = false
		end := min(i+opt.Batch, len(toSend))
		for _, t := range toSend[i:end] {
			presence := checkAgentOnSurface(t.UUID, probe, env.Sleep, env.PresenceInterval)
			if presence != Absent {
				if presence == Present {
					env.Logf("skip %s agent already running", t.Ref)
					delete(state.Attempts, t.UUID)
					delete(state.UnknownStreak, t.UUID)
				} else {
					env.Logf("skip %s cannot tell whether an agent is running", t.Ref)
				}
				continue
			}
			// Re-read the tree right before sending: a tty from the first read can be stale
			fresh, reason, unverifiable := env.recheckTab(t)
			if reason != "" {
				env.Logf("skip %s %s", t.Ref, reason)
				if unverifiable {
					state.UnknownStreak[t.UUID]++
				}
				continue
			}
			if fg := foregroundOnTty(env.Run, fresh.TTY, t.UUID); fg != FgShell {
				env.Logf("skip %s cannot confirm only a shell is in the foreground (%s)", t.Ref, fg)
				if fg == FgUnknown {
					state.UnknownStreak[t.UUID]++
				} else {
					delete(state.UnknownStreak, t.UUID)
				}
				continue
			}
			delete(state.UnknownStreak, t.UUID)
			if opt.DryRun {
				env.Logf("recheck %s no agent, shell in the foreground (dry-run, not sending)", t.Ref)
				continue
			}
			state.Attempts[t.UUID]++
			sentInPrevBatch = true
			if _, err := env.Run(env.Cmux, "send", "--surface", t.UUID, "cmux restore --surface "+t.UUID); err != nil {
				env.warn("send "+t.Ref, err)
				continue
			}
			if _, err := env.Run(env.Cmux, "send-key", "--surface", t.UUID, "enter"); err != nil {
				env.warn("send "+t.Ref, err)
				continue
			}
			env.Logf("sent %s %d/%d", t.Ref, state.Attempts[t.UUID], opt.MaxAttempts)
		}
		// Save after every batch so the attempt counts survive being stopped by a timeout
		save()
	}

	if opt.DryRun {
		return nil
	}

	// Show a status only in workspaces that have a tab a person must look at, and clear it elsewhere.
	// Two kinds, told apart in the text: tabs we gave up on (restore by hand), and tabs we could not
	// verify for unknownLimit runs in a row (check by hand).
	stuckByWs := map[string]int{}
	for _, t := range stuck {
		stuckByWs[t.WorkspaceRef]++
	}
	unverifiableByWs := map[string]int{}
	for _, t := range toSend {
		if state.UnknownStreak[t.UUID] >= unknownLimit {
			unverifiableByWs[t.WorkspaceRef]++
		}
	}
	for _, ws := range workspaceRefs {
		var parts []string
		if n := stuckByWs[ws]; n > 0 {
			parts = append(parts, fmt.Sprintf("Needs restore %d", n))
		}
		if n := unverifiableByWs[ws]; n > 0 {
			parts = append(parts, fmt.Sprintf("cannot verify %d", n))
		}
		if len(parts) > 0 {
			text := strings.Join(parts, ", ")
			if stuckByWs[ws] == 0 {
				text = "Cannot verify " + fmt.Sprint(unverifiableByWs[ws])
			}
			if _, err := env.Run(env.Cmux, "set-status", statusKey, text,
				"--workspace", ws, "--icon", "exclamationmark.triangle.fill", "--color", "#E5484D"); err != nil {
				env.warn("set-status "+ws, err)
			}
		} else if _, err := env.Run(env.Cmux, "clear-status", statusKey, "--workspace", ws); err != nil {
			env.warn("clear-status "+ws, err)
		}
	}
	state.LastCompletedRun = env.Now().Format(time.RFC3339)
	save()
	return nil
}
