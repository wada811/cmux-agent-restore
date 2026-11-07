package main

import (
	"fmt"
	"strings"
	"time"
)

// 既定値。フラグで上書きできる。
const (
	defaultBatch         = 3
	defaultBatchInterval = 20 * time.Second
	defaultMaxAttempts   = 3
	presenceInterval     = 1500 * time.Millisecond
	statusKey            = "agent_restore"
	// unknownLimit is how many runs in a row a tab may stay unverifiable before it is shown in the sidebar.
	unknownLimit = 3
)

// Options は run のフラグ。
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

// Env は run が外に出る口。試験では差し替える。
type Env struct {
	Run              Runner
	Cmux             string
	Sleep            func(time.Duration)
	Logf             func(format string, args ...any)
	StatePath        string
	Now              func() time.Time
	PresenceInterval time.Duration
}

// listTerminalTabs は全 workspace の ref と terminal タブの一覧を返す。cmux が応答しなければ ok=false。
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

// warn は失敗を WARN として記録する。
func (e *Env) warn(what string, err error) {
	msg := err.Error()
	if i := strings.Index(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	e.Logf("WARN %s failed: %s", what, msg)
}

// Restore は 1 回見回って、戻し損ねたタブに cmux restore を送る。
//
//   - 対象 = 復元記録（restore_record.checkpoint_id）があり、その kind が Kinds に含まれ、
//     エージェント（種類を問わない）のプロセスがいない terminal タブ。kind が不明・読めないタブは送らない
//   - エージェントの有無は tty で判定しない（`cmux tree` の tty は古いことがある）。
//     各 pid の環境変数 CMUX_SURFACE_ID を読み、tree の surface UUID と突き合わせる
//   - 対象の一覧は最初に 1 回だけ作り、送り終えるまで数分かかる。その間にエージェントが起動する
//     ので、タブごとに送る直前に checkAgentOnSurface で確かめ直し、いないと確かめられたタブにだけ送る。
//     加えて、そのタブの前面がシェルだけであること（foregroundOnTty）も確かめる。
//     いる・判定できない・前面がシェルでないタブはその回は送らず、試行回数にも数えない
//   - 一斉復元で pool_saturated になった実績があるので、Batch 件ずつ BatchInterval あけて送る
//   - 失敗の種類は見分けない。MaxAttempts 回送っても戻らないタブは送るのをやめる
//     （シェルに同じコマンドを打ち込み続けないため）。表示だけ、そのタブのある workspace の
//     サイドバーに agent_restore として残す。workspace の ref は cmux の再起動で変わるので、
//     どこに出したかは覚えず、毎回すべての workspace について出す・消すを決め直す
//   - cmux が応答しなければ何もしない
func Restore(env *Env, opt Options) error {
	workspaceRefs, tabs, ok := env.listTerminalTabs()
	if !ok {
		return nil
	}

	withAgent, err := surfacesWithAgent(env.Run)
	if err != nil {
		return fmt.Errorf("ps の実行に失敗: %w", err)
	}
	state := loadState(env.StatePath)

	// エージェントがいないタブのうち、対象の種類の復元記録があるものが対象。
	// 記録は、エージェントがいるタブでは読まない
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
	env.Logf("対象 %d 件（送信 %d・戻らない %d）", len(targets), len(toSend), len(stuck))
	for _, t := range targets {
		env.Logf("  %s %s %s 試行 %d/%d", t.Ref, t.UUID, kinds[t.UUID], state.Attempts[t.UUID], opt.MaxAttempts)
	}

	save := func() {
		if opt.DryRun {
			return
		}
		if err := saveState(env.StatePath, state); err != nil {
			env.warn("状態の保存", err)
		}
	}

	probe := func() (map[string]bool, error) { return surfacesWithAgent(env.Run) }
	sentInPrevBatch := false
	for i := 0; i < len(toSend); i += opt.Batch {
		// 送っていない組（エージェントが起動済み）の後は待たない。pool_saturated を避ける間隔は送信にだけ要る
		if sentInPrevBatch && !opt.DryRun {
			env.Sleep(opt.BatchInterval)
		}
		sentInPrevBatch = false
		end := min(i+opt.Batch, len(toSend))
		for _, t := range toSend[i:end] {
			presence := checkAgentOnSurface(t.UUID, probe, env.Sleep, env.PresenceInterval)
			if presence != Absent {
				if presence == Present {
					env.Logf("スキップ %s エージェントが起動済み", t.Ref)
					delete(state.Attempts, t.UUID)
					delete(state.UnknownStreak, t.UUID)
				} else {
					env.Logf("スキップ %s エージェントの有無を判定できない", t.Ref)
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
				env.Logf("スキップ %s 前面がシェルだけと確かめられない（%s）", t.Ref, fg)
				if fg == FgUnknown {
					state.UnknownStreak[t.UUID]++
				} else {
					delete(state.UnknownStreak, t.UUID)
				}
				continue
			}
			delete(state.UnknownStreak, t.UUID)
			if opt.DryRun {
				env.Logf("再確認 %s エージェントがいない・前面はシェル（dry-run のため送らない）", t.Ref)
				continue
			}
			state.Attempts[t.UUID]++
			sentInPrevBatch = true
			if _, err := env.Run(env.Cmux, "send", "--surface", t.UUID, "cmux restore --surface "+t.UUID); err != nil {
				env.warn("送信 "+t.Ref, err)
				continue
			}
			if _, err := env.Run(env.Cmux, "send-key", "--surface", t.UUID, "enter"); err != nil {
				env.warn("送信 "+t.Ref, err)
				continue
			}
			env.Logf("送信 %s %d/%d", t.Ref, state.Attempts[t.UUID], opt.MaxAttempts)
		}
		// timeout で止められても試行回数が残るよう、組ごとに書く
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
