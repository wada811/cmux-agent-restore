package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// world replaces cmux and ps. It holds the tab list, records, running agents and tty state,
// and records calls in order in events.
type world struct {
	mu sync.Mutex

	tabs      []wTab
	treeErr   error
	psErr     error
	kindsRead []string // refs of tabs whose restore record was read

	agents     map[string]bool   // surface UUIDs where an agent is running
	agentCmd   map[string]string // surface UUID -> agent command line (claude if empty)
	pidSurface map[string]string // pid -> surface UUID assigned in the latest ps -axo
	ttyEnv     map[string]string // tty -> output of ps eww -t
	ttyPs      map[string]string // tty -> output of ps -t
	psCalls    int               // number of ps -axo calls
	onPsCall   func(w *world, n int)
	treeCalls  int                   // number of cmux tree calls
	onTreeCall func(w *world, n int) // called right before returning the tree (n starts at 1)
	sendError  map[string]error      // ref -> send failure
	recordOut  map[string]string     // surface UUID -> replacement output of resume show
	recordErr  map[string]error      // surface UUID -> resume show failure

	events []string
}

type wTab struct {
	uuid, ref, ws, tty, kind string // no record if kind is empty
	browser                  bool
}

func tabN(i int, ws string) wTab {
	return wTab{
		uuid: fmt.Sprintf("%08X-0000-4000-8000-ABCDEF012345", i),
		ref:  fmt.Sprintf("surface:%d", i),
		ws:   ws,
		tty:  fmt.Sprintf("ttys%03d", i),
		kind: "claude",
	}
}

func newWorld(tabs ...wTab) *world {
	w := &world{tabs: tabs, agents: map[string]bool{}, agentCmd: map[string]string{}, ttyEnv: map[string]string{}, ttyPs: map[string]string{}, sendError: map[string]error{}, recordOut: map[string]string{}, recordErr: map[string]error{}}
	for _, t := range tabs {
		w.ttyPs[t.tty] = "1 Ss+ -/bin/zsh\n"
	}
	return w
}

func (w *world) treeJSON() string {
	var wsOrder []string
	byWs := map[string][]wTab{}
	for _, t := range w.tabs {
		if _, ok := byWs[t.ws]; !ok {
			wsOrder = append(wsOrder, t.ws)
		}
		byWs[t.ws] = append(byWs[t.ws], t)
	}
	var wss []string
	for _, ws := range wsOrder {
		var surfaces []string
		for _, t := range byWs[ws] {
			typ := "terminal"
			if t.browser {
				typ = "browser"
			}
			surfaces = append(surfaces, fmt.Sprintf(`{"id":%q,"ref":%q,"type":%q,"tty":%q}`, t.uuid, t.ref, typ, t.tty))
		}
		wss = append(wss, fmt.Sprintf(`{"ref":%q,"panes":[{"surfaces":[%s]}]}`, ws, strings.Join(surfaces, ",")))
	}
	return fmt.Sprintf(`{"windows":[{"workspaces":[%s]}]}`, strings.Join(wss, ","))
}

// refOf turns the UUID passed to --surface into a ref. If it is not a UUID, it adds "BAD:" so the test fails.
func (w *world) refOf(target string) string {
	for _, t := range w.tabs {
		if t.uuid == target {
			return t.ref
		}
	}
	return "BAD:" + target
}

func (w *world) record(e string) { w.events = append(w.events, e) }

func (w *world) run(name string, args ...string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if name == "ps" {
		return w.ps(args)
	}
	return w.cmux(args)
}

func (w *world) cmux(args []string) (string, error) {
	switch args[0] {
	case "tree":
		w.treeCalls++
		if w.onTreeCall != nil {
			w.onTreeCall(w, w.treeCalls)
		}
		if w.treeErr != nil {
			return "", w.treeErr
		}
		return w.treeJSON(), nil
	case "surface":
		// Pass a UUID to --surface. A ref can point at another tab after cmux restarts
		id := args[len(args)-1]
		if err := w.recordErr[id]; err != nil {
			return "", err
		}
		if out, ok := w.recordOut[id]; ok {
			return out, nil
		}
		for _, t := range w.tabs {
			if t.uuid == id {
				w.kindsRead = append(w.kindsRead, t.ref)
				if t.kind == "" {
					return `{"restore_record":null}`, nil
				}
				return fmt.Sprintf(`{"restore_record":{"checkpoint_id":"c","kind":%q}}`, t.kind), nil
			}
		}
		return "", errors.New("no such surface")
	case "send":
		ref := w.refOf(args[2])
		if err := w.sendError[ref]; err != nil {
			return "", err
		}
		w.record("send " + ref + " " + args[3])
	case "send-key":
		w.record("send-key " + w.refOf(args[2]) + " " + args[3])
	case "set-status":
		w.record("set-status " + args[2] + " " + args[4])
	case "clear-status":
		w.record("clear-status " + args[3])
	}
	return "", nil
}

func (w *world) ps(args []string) (string, error) {
	if w.psErr != nil {
		return "", w.psErr
	}
	switch {
	case args[0] == "-axo":
		w.psCalls++
		if w.onPsCall != nil {
			w.onPsCall(w, w.psCalls)
		}
		// Assign a pid to each surface with an agent. ps eww -p looks up pid -> surface
		w.pidSurface = map[string]string{}
		var lines []string
		pid := 1000
		for _, t := range w.tabs {
			if w.agents[t.uuid] {
				pid++
				w.pidSurface[fmt.Sprint(pid)] = t.uuid
				cmd := w.agentCmd[t.uuid]
				if cmd == "" {
					cmd = "/Users/andy/.local/bin/claude --resume x"
				}
				lines = append(lines, fmt.Sprintf("%d %s", pid, cmd))
			}
		}
		return strings.Join(lines, "\n"), nil
	case args[0] == "eww" && args[1] == "-p":
		// Return only the lines of the pids passed (like the real ps)
		var lines []string
		for _, pid := range strings.Split(args[2], ",") {
			if u, ok := w.pidSurface[pid]; ok {
				lines = append(lines, "agent CMUX_SURFACE_ID="+u)
			}
		}
		return strings.Join(lines, "\n"), nil
	case args[0] == "eww" && args[1] == "-t":
		return w.ttyEnv[args[2]], nil
	case args[0] == "-t":
		out, ok := w.ttyPs[args[1]]
		if !ok {
			return "", errors.New("exit 1")
		}
		return out, nil
	}
	return "", fmt.Errorf("unexpected ps: %v", args)
}

func (w *world) sends() []string {
	var out []string
	for _, e := range w.events {
		if strings.HasPrefix(e, "send ") {
			out = append(out, strings.Fields(e)[1])
		}
	}
	return out
}

type harness struct {
	w      *world
	env    *Env
	logs   []string
	sleeps []time.Duration
}

func newHarness(t *testing.T, w *world) *harness {
	h := &harness{w: w}
	h.env = &Env{
		Run:  w.run,
		Cmux: "/fake/cmux",
		Sleep: func(d time.Duration) {
			h.sleeps = append(h.sleeps, d)
			// Record only the batch pauses in events, so their order against the sends can be checked
			if d == defaultBatchInterval || d == 5*time.Second {
				w.record(fmt.Sprintf("sleep %s", d))
			}
		},
		Logf:             func(f string, a ...any) { h.logs = append(h.logs, fmt.Sprintf(f, a...)) },
		StatePath:        filepath.Join(t.TempDir(), "state.json"),
		Now:              func() time.Time { return time.Date(2026, 11, 7, 17, 0, 0, 0, time.UTC) },
		PresenceInterval: presenceInterval,
	}
	return h
}

func (h *harness) logged(sub string) bool {
	return slices.ContainsFunc(h.logs, func(l string) bool { return strings.Contains(l, sub) })
}

func (h *harness) count(d time.Duration) int {
	n := 0
	for _, s := range h.sleeps {
		if s == d {
			n++
		}
	}
	return n
}

func mustRestore(t *testing.T, h *harness, opt Options) {
	t.Helper()
	if err := Restore(h.env, opt); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreBatches(t *testing.T) {
	var tabs []wTab
	for i := 1; i <= 7; i++ {
		tabs = append(tabs, tabN(i, "workspace:1"))
	}
	w := newWorld(tabs...)
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())

	if got := len(w.sends()); got != 7 {
		t.Fatalf("%d sends, want 7", got)
	}
	// Batches of 3, 3 and 1. Pause 20 seconds between batches (between the 3rd and 4th, and the 6th and 7th)
	var kinds []string
	for _, e := range w.events {
		switch {
		case strings.HasPrefix(e, "send "):
			kinds = append(kinds, "s")
		case strings.HasPrefix(e, "sleep "):
			kinds = append(kinds, "G")
		}
	}
	if got, want := strings.Join(kinds, ""), "sssGsssGs"; got != want {
		t.Errorf("order of sends and pauses %q, want %q", got, want)
	}
	if got := h.count(20 * time.Second); got != 2 {
		t.Errorf("%d pauses of 20 seconds, want 2", got)
	}
	if !slices.Contains(w.events, "send-key surface:1 enter") {
		t.Errorf("enter was not sent: %v", w.events)
	}
	if !slices.Contains(w.events, "send surface:1 cmux restore --surface "+tabs[0].uuid) {
		t.Errorf("wrong restore command: %v", w.events)
	}
	state := loadState(h.env.StatePath)
	if state.Attempts[tabs[0].uuid] != 1 || state.Attempts[tabs[6].uuid] != 1 {
		t.Errorf("attempt counts were not recorded: %v", state.Attempts)
	}
	if state.LastCompletedRun != "2026-11-07T17:00:00Z" {
		t.Errorf("last_completed_run = %q", state.LastCompletedRun)
	}
}

func TestRestoreBatchOptions(t *testing.T) {
	var tabs []wTab
	for i := 1; i <= 5; i++ {
		tabs = append(tabs, tabN(i, "workspace:1"))
	}
	w := newWorld(tabs...)
	h := newHarness(t, w)
	opt := defaultOptions()
	opt.Batch = 2
	opt.BatchInterval = 5 * time.Second
	mustRestore(t, h, opt)
	if got := h.count(5 * time.Second); got != 2 {
		t.Errorf("%d pauses of 5 seconds, want 2 (batches of 2, 2, 1)", got)
	}
}

func TestRestoreNoWaitWhenBatchSentNothing(t *testing.T) {
	var tabs []wTab
	for i := 1; i <= 6; i++ {
		tabs = append(tabs, tabN(i, "workspace:1"))
	}
	w := newWorld(tabs...)
	for _, t := range tabs {
		w.ttyPs[t.tty] = "1 Ss -/bin/zsh\n2 S+ vim\n" // an editor is in front on every tab
	}
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.sends()) != 0 {
		t.Errorf("sent: %v", w.sends())
	}
	if got := h.count(20 * time.Second); got != 0 {
		t.Errorf("waited 20 seconds although nothing was sent (%d times)", got)
	}
}

func TestRestoreStopsAtMaxAttempts(t *testing.T) {
	a, b, c := tabN(1, "workspace:1"), tabN(2, "workspace:1"), tabN(3, "workspace:2")
	w := newWorld(a, b, c)
	h := newHarness(t, w)
	if err := saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 3, b.uuid: 1}}); err != nil {
		t.Fatal(err)
	}
	mustRestore(t, h, defaultOptions())

	if got := w.sends(); !slices.Equal(got, []string{"surface:2", "surface:3"}) {
		t.Errorf("sent to %v, want surface:2 surface:3 (no send to the tab already sent 3 times)", got)
	}
	state := loadState(h.env.StatePath)
	if state.Attempts[a.uuid] != 3 || state.Attempts[b.uuid] != 2 || state.Attempts[c.uuid] != 1 {
		t.Errorf("attempts %v", state.Attempts)
	}
	if !slices.Contains(w.events, "set-status Needs restore 1 workspace:1") {
		t.Errorf("not shown in the sidebar: %v", w.events)
	}
	if !slices.Contains(w.events, "clear-status workspace:2") {
		t.Errorf("did not clear the display of a workspace with nothing given up: %v", w.events)
	}
	if !h.logged("targets 3 (to send 2, given up 1)") {
		t.Errorf("log %v", h.logs)
	}
}

func TestRestoreMaxAttemptsOption(t *testing.T) {
	a := tabN(1, "workspace:1")
	w := newWorld(a)
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 1}})
	opt := defaultOptions()
	opt.MaxAttempts = 1
	mustRestore(t, h, opt)
	if len(w.sends()) != 0 {
		t.Errorf("sent although the limit is 1: %v", w.sends())
	}
}

func TestRestoreCleansState(t *testing.T) {
	a, gone, back := tabN(1, "workspace:1"), tabN(2, "workspace:1"), tabN(3, "workspace:1")
	w := newWorld(a, back)
	w.agents[back.uuid] = true // a tab that came back
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 1, gone.uuid: 2, back.uuid: 2}})
	mustRestore(t, h, defaultOptions())
	state := loadState(h.env.StatePath)
	if _, ok := state.Attempts[gone.uuid]; ok {
		t.Error("a tab that disappeared is still in the state")
	}
	if _, ok := state.Attempts[back.uuid]; ok {
		t.Error("a tab that came back is still in the state")
	}
	if state.Attempts[a.uuid] != 2 {
		t.Errorf("a = %d, want 2", state.Attempts[a.uuid])
	}
}

func TestRestoreDryRun(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 3}})
	before := loadState(h.env.StatePath)
	opt := defaultOptions()
	opt.DryRun = true
	mustRestore(t, h, opt)

	if len(w.events) != 0 {
		t.Errorf("dry-run wrote to cmux: %v", w.events)
	}
	if after := loadState(h.env.StatePath); after.LastCompletedRun != before.LastCompletedRun || after.Attempts[a.uuid] != 3 || len(after.Attempts) != 1 {
		t.Errorf("dry-run changed the state: %v", after)
	}
	if !h.logged("recheck surface:2 no agent, shell in the foreground (dry-run, not sending)") {
		t.Errorf("log %v", h.logs)
	}
	if got := h.count(20 * time.Second); got != 0 {
		t.Errorf("dry-run waited 20 seconds")
	}
}

func TestRestoreDryRunDoesNotCreateStateFile(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"))
	h := newHarness(t, w)
	opt := defaultOptions()
	opt.DryRun = true
	mustRestore(t, h, opt)
	if m, _ := filepath.Glob(h.env.StatePath); len(m) != 0 {
		t.Errorf("dry-run created a state file")
	}
}

func TestRestoreSkipsWhenAgentStartedBeforeSend(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	// The first ps (building the list) does not see it; the check right before sending (second) sees an agent already on a
	w.onPsCall = func(w *world, n int) {
		if n == 2 {
			w.agents[a.uuid] = true
		}
	}
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 1}})
	mustRestore(t, h, defaultOptions())

	if got := w.sends(); !slices.Equal(got, []string{"surface:2"}) {
		t.Errorf("sent to %v, want surface:2 only", got)
	}
	state := loadState(h.env.StatePath)
	if _, ok := state.Attempts[a.uuid]; ok {
		t.Errorf("the attempt count of a tab whose agent started remains: %v", state.Attempts)
	}
	if !h.logged("skip surface:1 agent already running") {
		t.Errorf("log %v", h.logs)
	}
}

func TestRestoreSkipsWhenPresenceUnknown(t *testing.T) {
	a := tabN(1, "workspace:1")
	w := newWorld(a)
	// The ps that builds the list passes; the ps fails in the check right before sending
	w.onPsCall = func(w *world, n int) {
		if n >= 2 {
			w.psErr = errors.New("ps failed")
		}
	}
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.sends()) != 0 {
		t.Errorf("sent although it could not be decided: %v", w.sends())
	}
	if state := loadState(h.env.StatePath); state.Attempts[a.uuid] != 0 {
		t.Errorf("counted as an attempt: %v", state.Attempts)
	}
}

func TestRestoreSkipsWhenForegroundNotShell(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	w.ttyPs[a.tty] = "1 Ss -/bin/zsh\n2 S+ vim\n"
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 1}})
	mustRestore(t, h, defaultOptions())
	if got := w.sends(); !slices.Equal(got, []string{"surface:2"}) {
		t.Errorf("sent to %v", got)
	}
	if got := loadState(h.env.StatePath).Attempts[a.uuid]; got != 1 {
		t.Errorf("attempts of a tab whose foreground is not a shell is %d (must stay 1)", got)
	}
	if !h.logged("skip surface:1 cannot confirm only a shell is in the foreground (busy)") {
		t.Errorf("log %v", h.logs)
	}
}

func TestRestoreSkipsWhenTtyPointsToOtherTab(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	w.ttyEnv[a.tty] = "vim CMUX_SURFACE_ID=4DE01DAC-A9A7-4308-939C-763E5880CFA0" // stale tty
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := w.sends(); !slices.Equal(got, []string{"surface:2"}) {
		t.Errorf("sent to %v", got)
	}
	if got := loadState(h.env.StatePath).Attempts[a.uuid]; got != 0 {
		t.Errorf("attempts of a tab whose tty points at another tab is %d", got)
	}
	if !h.logged("skip surface:1 cannot confirm only a shell is in the foreground (unknown)") {
		t.Errorf("log %v", h.logs)
	}
}

func TestRestoreSkipsWhenNoTty(t *testing.T) {
	a := tabN(1, "workspace:1")
	a.tty = ""
	w := newWorld(a)
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.sends()) != 0 {
		t.Errorf("sent although there is no tty")
	}
}

func TestRestoreSelectsByKind(t *testing.T) {
	a, b, c, d := tabN(1, "workspace:1"), tabN(2, "workspace:1"), tabN(3, "workspace:1"), tabN(4, "workspace:1")
	b.kind = "codex"
	c.kind = "gemini"
	d.kind = "" // no record
	br := tabN(5, "workspace:1")
	br.browser = true
	w := newWorld(a, b, c, d, br)
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := w.sends(); !slices.Equal(got, []string{"surface:1", "surface:2"}) {
		t.Errorf("sent to %v", got)
	}
}

func TestRestoreKindsOption(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	b.kind = "opencode"
	w := newWorld(a, b)
	h := newHarness(t, w)
	opt := defaultOptions()
	opt.Kinds = []string{"opencode"}
	mustRestore(t, h, opt)
	if got := w.sends(); !slices.Equal(got, []string{"surface:2"}) {
		t.Errorf("sent to %v", got)
	}
}

func TestRestoreDoesNotReadRecordOfTabWithAgent(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	w.agents[a.uuid] = true
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if slices.Contains(w.kindsRead, "surface:1") {
		t.Error("read the restore record of a tab with an agent")
	}
}

func TestRestoreDoesNothingWhenCmuxDoesNotRespond(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"))
	w.treeErr = errors.New("cmux does not answer")
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.events) != 0 || w.psCalls != 0 || len(h.logs) != 0 {
		t.Errorf("should have done nothing but acted: events=%v ps=%d logs=%v", w.events, w.psCalls, h.logs)
	}
}

func TestRestoreFailsWhenPsFails(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"))
	w.psErr = errors.New("ps failed")
	h := newHarness(t, w)
	if err := Restore(h.env, defaultOptions()); err == nil {
		t.Error("ps failed but no error was returned")
	}
	if len(w.sends()) != 0 {
		t.Error("sent although ps failed")
	}
}

func TestRestoreCountsAttemptEvenWhenSendFails(t *testing.T) {
	a := tabN(1, "workspace:1")
	w := newWorld(a)
	w.sendError[a.ref] = errors.New("send failed")
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := loadState(h.env.StatePath).Attempts[a.uuid]; got != 1 {
		t.Errorf("a failed send still counts as an attempt (the command may have reached the shell): %d", got)
	}
	if !h.logged("WARN send surface:1 failed") {
		t.Errorf("log %v", h.logs)
	}
}

func TestRestoreClearsStatusInEveryWorkspace(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:2")
	w := newWorld(a, b)
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	for _, ws := range []string{"workspace:1", "workspace:2"} {
		if !slices.Contains(w.events, "clear-status "+ws) {
			t.Errorf("did not clear the display of %s: %v", ws, w.events)
		}
	}
}

func TestRestoreDoesNotSendToTabRunningCodex(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	a.kind = "codex"
	w := newWorld(a, b)
	w.agents[a.uuid] = true
	w.agentCmd[a.uuid] = "/Users/andy/.local/bin/codex resume x"
	// A tab with Codex in front. The record stays codex
	w.ttyPs[a.tty] = "1 Ss -/bin/zsh\n2 S+ codex\n"
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := w.sends(); !slices.Equal(got, []string{"surface:2"}) {
		t.Errorf("sent to %v, want [surface:2] (no send to a tab where Codex is running)", got)
	}
}

func TestRestoreSkipsWhenAgentAppearsOnlyAtSecondCheck(t *testing.T) {
	a := tabN(1, "workspace:1")
	w := newWorld(a)
	// ps -axo is called in this order: the first list (1st), the first check before sending (2nd), the second check (3rd).
	// The agent becomes visible only on the 3rd
	w.onPsCall = func(w *world, n int) {
		if n == 3 {
			w.agents[a.uuid] = true
		}
	}
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.sends()) != 0 {
		t.Errorf("sent although the second check saw an agent: %v", w.sends())
	}
	if !h.logged("agent already running") {
		t.Errorf("log %v", h.logs)
	}
	if n := loadState(h.env.StatePath).Attempts[a.uuid]; n != 0 {
		t.Errorf("attempts %d, want 0", n)
	}
}

func TestRestoreSkipsTabsSharingATty(t *testing.T) {
	// Login and zsh lines carry no CMUX_SURFACE_ID, so a stale tty of another tab cannot be told apart by ps alone.
	// If two or more tabs in the tree claim the same tty, we cannot tell which is real, so do not send
	a, b, c := tabN(1, "workspace:1"), tabN(2, "workspace:1"), tabN(3, "workspace:1")
	b.tty = a.tty
	w := newWorld(a, b, c)
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := w.sends(); !slices.Equal(got, []string{"surface:3"}) {
		t.Errorf("sent to %v, want [surface:3]", got)
	}
	if !h.logged("share a tty") {
		t.Errorf("log %v", h.logs)
	}
	if n := loadState(h.env.StatePath).Attempts[a.uuid]; n != 0 {
		t.Errorf("attempts of a tab not sent to %d, want 0", n)
	}
}

func TestRestoreSkipsWhenTtyChangedSinceFirstTree(t *testing.T) {
	a := tabN(1, "workspace:1")
	w := newWorld(a)
	// The tty changes after the first tree (the re-read right before sending)
	w.onTreeCall = func(w *world, n int) {
		if n >= 2 {
			w.tabs[0].tty = "ttys999"
			w.ttyPs["ttys999"] = "1 Ss+ -/bin/zsh\n"
		}
	}
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.sends()) != 0 {
		t.Errorf("sent although the tty changed: %v", w.sends())
	}
	if !h.logged("tty changed since") {
		t.Errorf("log %v", h.logs)
	}
}

func TestRestoreSkipsWhenTabDisappearedSinceFirstTree(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	w.onTreeCall = func(w *world, n int) {
		if n == 2 {
			w.tabs = w.tabs[1:] // surface:1 was closed
		}
	}
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := w.sends(); !slices.Equal(got, []string{"surface:2"}) {
		t.Errorf("sent to %v, want [surface:2]", got)
	}
	if !h.logged("no longer exists") {
		t.Errorf("log %v", h.logs)
	}
}

func TestRestoreKeepsAttemptsWhenRecordCannotBeRead(t *testing.T) {
	// "Could not read the record" differs from "no record". If the attempt count of an unreadable tab were removed,
	// we would send again to a tab we had given up on
	a, b, c := tabN(1, "workspace:1"), tabN(2, "workspace:1"), tabN(3, "workspace:1")
	w := newWorld(a, b, c)
	w.recordErr[a.uuid] = errors.New("cmux timed out")
	w.recordOut[b.uuid] = "not json"
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 3, b.uuid: 2, c.uuid: 1}})
	mustRestore(t, h, defaultOptions())
	state := loadState(h.env.StatePath)
	if state.Attempts[a.uuid] != 3 || state.Attempts[b.uuid] != 2 {
		t.Errorf("the attempt count of an unreadable tab was removed or changed: %v", state.Attempts)
	}
	if got := w.sends(); !slices.Equal(got, []string{"surface:3"}) {
		t.Errorf("sent to %v, want [surface:3] (no send to an unreadable tab)", got)
	}
	if !h.logged("could not read the restore record") {
		t.Errorf("log %v", h.logs)
	}
}

func TestRestoreCleansAttemptsWhenRecordIsGone(t *testing.T) {
	a := tabN(1, "workspace:1")
	a.kind = "" // no record (read fine, and null)
	w := newWorld(a)
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 2}})
	mustRestore(t, h, defaultOptions())
	if _, ok := loadState(h.env.StatePath).Attempts[a.uuid]; ok {
		t.Error("the attempt count of a tab whose record is gone remains")
	}
}

func TestRestoreShowsTabsThatStayUnverifiable(t *testing.T) {
	// Do not send to a tab whose foreground cannot be confirmed (the tty is unknown). So that a person notices if this goes on,
	// show it in the sidebar after 3 runs in a row, worded differently from tabs to restore by hand (Needs restore)
	a := tabN(1, "workspace:1")
	a.tty = ""
	w := newWorld(a)
	h := newHarness(t, w)
	for run := 1; run <= 3; run++ {
		w.events = nil
		mustRestore(t, h, defaultOptions())
		state := loadState(h.env.StatePath)
		if state.UnknownStreak[a.uuid] != run {
			t.Fatalf("run %d: unknown_streak = %v", run, state.UnknownStreak)
		}
		shown := slices.Contains(w.events, "set-status Cannot verify 1 workspace:1")
		if shown != (run == 3) {
			t.Errorf("run %d: sidebar = %v, events = %v", run, shown, w.events)
		}
	}
	if len(w.sends()) != 0 {
		t.Errorf("sent to a tab that could not be verified: %v", w.sends())
	}
	if n := loadState(h.env.StatePath).Attempts[a.uuid]; n != 0 {
		t.Errorf("attempts %d, want 0", n)
	}
}

func TestRestoreResetsUnverifiableCountWhenForegroundIsKnown(t *testing.T) {
	a := tabN(1, "workspace:1")
	w := newWorld(a)
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{}, UnknownStreak: map[string]int{a.uuid: 2}})
	mustRestore(t, h, defaultOptions()) // the foreground is confirmed as a shell, so it sends
	if _, ok := loadState(h.env.StatePath).UnknownStreak[a.uuid]; ok {
		t.Error("the streak of a tab that was verified remains")
	}
}

func TestRestoreCountsSharedTtyAsUnverifiable(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	b.tty = a.tty
	w := newWorld(a, b)
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{}, UnknownStreak: map[string]int{a.uuid: 2, b.uuid: 2}})
	mustRestore(t, h, defaultOptions())
	if !slices.Contains(w.events, "set-status Cannot verify 2 workspace:1") {
		t.Errorf("events = %v", w.events)
	}
}

func TestRestoreSidebarSeparatesStuckFromUnverifiable(t *testing.T) {
	stuck, unknown := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	unknown.tty = ""
	w := newWorld(stuck, unknown)
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{
		Attempts:      map[string]int{stuck.uuid: 3},
		UnknownStreak: map[string]int{unknown.uuid: 2},
	})
	mustRestore(t, h, defaultOptions())
	if !slices.Contains(w.events, "set-status Needs restore 1, cannot verify 1 workspace:1") {
		t.Errorf("events = %v", w.events)
	}
}
