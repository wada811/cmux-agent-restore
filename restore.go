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
	defaultRetryInterval = 10 * time.Second
	defaultMaxRuntime    = 4 * time.Minute
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
	// RetryInterval is the wait before tabs skipped in this run are checked again. 0 turns retrying off.
	RetryInterval time.Duration
	// MaxRuntime caps one run, so it ends before the next launchd run starts.
	MaxRuntime time.Duration
}

func defaultOptions() Options {
	return Options{Batch: defaultBatch, BatchInterval: defaultBatchInterval, MaxAttempts: defaultMaxAttempts, Kinds: DefaultKinds, RetryInterval: defaultRetryInterval, MaxRuntime: defaultMaxRuntime}
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
// but we cannot tell which terminal it owns. retryable is true when the reason can pass within the
// same run: a tree that could not be read, or a tty still being reassigned after a restart. A gone tab
// and a tty claimed by other tabs do not pass within minutes, so they wait for the next run.
func (e *Env) recheckTab(t Tab) (fresh Tab, reason string, unverifiable, retryable bool) {
	_, tabs, ok := e.listTerminalTabs()
	if !ok {
		return Tab{}, "cannot re-read the tab list", false, true
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
		return Tab{}, "tab no longer exists", false, false
	case cur.TTY != t.TTY:
		return Tab{}, "tty changed since the first read (" + t.TTY + " -> " + cur.TTY + ")", true, true
	case sharedTTYs(tabs)[cur.TTY]:
		return Tab{}, "other tabs share a tty with this tab (" + cur.TTY + ")", true, false
	}
	return *cur, "", false, false
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
//   - Tabs skipped for a reason that can pass within minutes are checked again in the same run,
//     RetryInterval later, with the same checks, until they pass or the run reaches MaxRuntime. Not retried
//     in the same run: an agent already running, a tab that is gone, and a tty other tabs also claim (it
//     stays that way until cmux reassigns ttys, so retrying only keeps the run going to MaxRuntime).
//     Whatever is left then waits for the next run. UnknownStreak grows at most once per run per tab.
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
	deadline := env.Now().Add(opt.MaxRuntime)
	streakBumped := map[string]bool{} // UnknownStreak grows at most once per run, however often a tab is retried
	bumpStreak := func(uuid string) {
		if !streakBumped[uuid] {
			streakBumped[uuid] = true
			state.UnknownStreak[uuid]++
		}
	}
	// attempt runs the checks for one tab and sends if they all pass. retry is true when the tab was
	// skipped for a reason that can pass later (cmux has just restarted and ttys are still being assigned).
	// sent is true when a command was typed into the tab.
	attempt := func(t Tab) (retry, sent bool) {
		presence := checkAgentOnSurface(t.UUID, probe, env.Sleep, env.PresenceInterval)
		if presence != Absent {
			if presence == Present {
				env.Logf("skip %s agent already running", t.Ref)
				delete(state.Attempts, t.UUID)
				delete(state.UnknownStreak, t.UUID)
				return false, false
			}
			env.Logf("skip %s cannot tell whether an agent is running", t.Ref)
			return true, false
		}
		// Re-read the tree right before sending: a tty from the first read can be stale
		fresh, reason, unverifiable, retryable := env.recheckTab(t)
		if reason != "" {
			env.Logf("skip %s %s", t.Ref, reason)
			if unverifiable {
				bumpStreak(t.UUID)
			}
			return retryable, false
		}
		if fg := foregroundOnTty(env.Run, fresh.TTY, t.UUID); fg != FgShell {
			env.Logf("skip %s cannot confirm only a shell is in the foreground (%s)", t.Ref, fg)
			if fg == FgUnknown {
				bumpStreak(t.UUID)
			} else {
				delete(state.UnknownStreak, t.UUID)
			}
			return true, false
		}
		delete(state.UnknownStreak, t.UUID)
		if opt.DryRun {
			env.Logf("recheck %s no agent, shell in the foreground (dry-run, not sending)", t.Ref)
			return false, false
		}
		state.Attempts[t.UUID]++
		if _, err := env.Run(env.Cmux, "send", "--surface", t.UUID, "cmux restore --surface "+t.UUID); err != nil {
			env.warn("send "+t.Ref, err)
			return false, true
		}
		if _, err := env.Run(env.Cmux, "send-key", "--surface", t.UUID, "enter"); err != nil {
			env.warn("send "+t.Ref, err)
			return false, true
		}
		env.Logf("sent %s %d/%d", t.Ref, state.Attempts[t.UUID], opt.MaxAttempts)
		return false, true
	}

	// Round 0 goes through every tab. Tabs skipped for a reason that can pass later are tried again in
	// the next round, after RetryInterval, until none are left or the run reaches MaxRuntime.
	pending := toSend
	sentInPrevBatch := false
	timedOut := false
	for round := 0; len(pending) > 0 && !timedOut; round++ {
		if round > 0 {
			// The pause that avoids pool_saturated also covers the retry wait
			wait := opt.RetryInterval
			if sentInPrevBatch {
				wait = max(wait, opt.BatchInterval)
				sentInPrevBatch = false
			}
			if env.Now().Add(wait + env.PresenceInterval).After(deadline) {
				timedOut = true
				break
			}
			env.Sleep(wait)
			env.Logf("retry %d: %d tabs skipped earlier in this run", round, len(pending))
		}
		var retryList []Tab
		for i := 0; i < len(pending) && !timedOut; i += opt.Batch {
			// Do not wait after a batch that sent nothing (agents had already started). The pause that avoids pool_saturated is only needed after sending
			if sentInPrevBatch && !opt.DryRun {
				if env.Now().Add(opt.BatchInterval + env.PresenceInterval).After(deadline) {
					timedOut = true
					retryList = append(retryList, pending[i:]...)
					break
				}
				env.Sleep(opt.BatchInterval)
			}
			sentInPrevBatch = false
			end := min(i+opt.Batch, len(pending))
			for j, t := range pending[i:end] {
				if !env.Now().Before(deadline) {
					timedOut = true
					retryList = append(retryList, pending[i+j:]...)
					break
				}
				retry, sent := attempt(t)
				if sent {
					sentInPrevBatch = true
				}
				if retry {
					retryList = append(retryList, t)
				}
			}
			// Save after every batch so the attempt counts survive being stopped by a timeout
			save()
		}
		if opt.RetryInterval <= 0 {
			// Retrying is off
			pending = nil
			break
		}
		// Tabs that were never looked at in this round are in retryList too
		pending = retryList
	}
	if timedOut && len(pending) > 0 {
		env.Logf("time limit %s reached, %d tabs left for the next run", opt.MaxRuntime, len(pending))
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
