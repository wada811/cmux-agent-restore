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

// world は cmux と ps の差し替え。タブの一覧・記録・動いているエージェント・tty の状態を持ち、
// 呼び出しを events に順に残す。
type world struct {
	mu sync.Mutex

	tabs      []wTab
	treeErr   error
	psErr     error
	kindsRead []string // 復元記録を読んだタブの ref

	agents    map[string]bool   // 動いているエージェントがいる surface UUID
	ttyEnv    map[string]string // tty → ps eww -t の出力
	ttyPs     map[string]string // tty → ps -t の出力
	psCalls   int               // ps -axo の呼び出し回数
	onPsCall  func(w *world, n int)
	sendError map[string]error // ref → send の失敗

	events []string
}

type wTab struct {
	uuid, ref, ws, tty, kind string // kind が空なら記録なし
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
	w := &world{tabs: tabs, agents: map[string]bool{}, ttyEnv: map[string]string{}, ttyPs: map[string]string{}, sendError: map[string]error{}}
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

// refOf は --surface に渡された UUID を ref にする。UUID でなければ「BAD:」を付けて、試験が失敗するようにする。
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
		if w.treeErr != nil {
			return "", w.treeErr
		}
		return w.treeJSON(), nil
	case "surface":
		// --surface には UUID を渡す。ref だと cmux の再起動で別のタブを指すことがある
		id := args[len(args)-1]
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
		var lines []string
		pid := 1000
		for _, t := range w.tabs {
			if w.agents[t.uuid] {
				pid++
				lines = append(lines, fmt.Sprintf("%d /Users/andy/.local/bin/claude --resume x", pid))
			}
		}
		return strings.Join(lines, "\n"), nil
	case args[0] == "eww" && args[1] == "-p":
		var lines []string
		for u := range w.agents {
			if w.agents[u] {
				lines = append(lines, "claude CMUX_SURFACE_ID="+u)
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
	return "", fmt.Errorf("想定外の ps: %v", args)
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
			// 組の間隔だけを events に残して、送信との前後を確かめられるようにする
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
		t.Fatalf("送信 %d 件, want 7", got)
	}
	// 3 件・3 件・1 件の組。組の間（3 件目と 4 件目、6 件目と 7 件目）に 20 秒あける
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
		t.Errorf("送信と間隔の並び %q, want %q", got, want)
	}
	if got := h.count(20 * time.Second); got != 2 {
		t.Errorf("20 秒の間隔 %d 回, want 2", got)
	}
	if !slices.Contains(w.events, "send-key surface:1 enter") {
		t.Errorf("enter を送っていない: %v", w.events)
	}
	if !slices.Contains(w.events, "send surface:1 cmux restore --surface "+tabs[0].uuid) {
		t.Errorf("復元コマンドが違う: %v", w.events)
	}
	state := loadState(h.env.StatePath)
	if state.Attempts[tabs[0].uuid] != 1 || state.Attempts[tabs[6].uuid] != 1 {
		t.Errorf("試行回数が記録されていない: %v", state.Attempts)
	}
	if state.LastRun != "2026-11-07T17:00:00Z" {
		t.Errorf("last_run = %q", state.LastRun)
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
		t.Errorf("5 秒の間隔 %d 回, want 2（2・2・1 件の組）", got)
	}
}

func TestRestoreNoWaitWhenBatchSentNothing(t *testing.T) {
	var tabs []wTab
	for i := 1; i <= 6; i++ {
		tabs = append(tabs, tabN(i, "workspace:1"))
	}
	w := newWorld(tabs...)
	for _, t := range tabs {
		w.ttyPs[t.tty] = "1 Ss -/bin/zsh\n2 S+ vim\n" // 全タブでエディタが前面
	}
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.sends()) != 0 {
		t.Errorf("送った: %v", w.sends())
	}
	if got := h.count(20 * time.Second); got != 0 {
		t.Errorf("何も送っていないのに 20 秒待った（%d 回）", got)
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
		t.Errorf("送信先 %v, want surface:2 surface:3（3 回送ったタブには送らない）", got)
	}
	state := loadState(h.env.StatePath)
	if state.Attempts[a.uuid] != 3 || state.Attempts[b.uuid] != 2 || state.Attempts[c.uuid] != 1 {
		t.Errorf("試行回数 %v", state.Attempts)
	}
	if !slices.Contains(w.events, "set-status Needs restore 1 workspace:1") {
		t.Errorf("サイドバーに出していない: %v", w.events)
	}
	if !slices.Contains(w.events, "clear-status workspace:2") {
		t.Errorf("打ち切りのない workspace の表示を消していない: %v", w.events)
	}
	if !h.logged("対象 3 件（送信 2・戻らない 1）") {
		t.Errorf("ログ %v", h.logs)
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
		t.Errorf("1 回で打ち切る設定なのに送った: %v", w.sends())
	}
}

func TestRestoreCleansState(t *testing.T) {
	a, gone, back := tabN(1, "workspace:1"), tabN(2, "workspace:1"), tabN(3, "workspace:1")
	w := newWorld(a, back)
	w.agents[back.uuid] = true // 戻ったタブ
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 1, gone.uuid: 2, back.uuid: 2}})
	mustRestore(t, h, defaultOptions())
	state := loadState(h.env.StatePath)
	if _, ok := state.Attempts[gone.uuid]; ok {
		t.Error("消えたタブが状態に残っている")
	}
	if _, ok := state.Attempts[back.uuid]; ok {
		t.Error("戻ったタブが状態に残っている")
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
		t.Errorf("dry-run で cmux に書いた: %v", w.events)
	}
	if after := loadState(h.env.StatePath); after.LastRun != before.LastRun || after.Attempts[a.uuid] != 3 || len(after.Attempts) != 1 {
		t.Errorf("dry-run で状態が変わった: %v", after)
	}
	if !h.logged("surface:2 エージェントがいない・前面はシェル（dry-run のため送らない）") {
		t.Errorf("ログ %v", h.logs)
	}
	if got := h.count(20 * time.Second); got != 0 {
		t.Errorf("dry-run で 20 秒待った")
	}
}

func TestRestoreDryRunDoesNotCreateStateFile(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"))
	h := newHarness(t, w)
	opt := defaultOptions()
	opt.DryRun = true
	mustRestore(t, h, opt)
	if m, _ := filepath.Glob(h.env.StatePath); len(m) != 0 {
		t.Errorf("dry-run が状態ファイルを作った")
	}
}

func TestRestoreSkipsWhenAgentStartedBeforeSend(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	// 1 回目の ps（一覧作り）では見えず、送る直前の確認（2 回目）で a に起動済み
	w.onPsCall = func(w *world, n int) {
		if n == 2 {
			w.agents[a.uuid] = true
		}
	}
	h := newHarness(t, w)
	_ = saveState(h.env.StatePath, State{Attempts: map[string]int{a.uuid: 1}})
	mustRestore(t, h, defaultOptions())

	if got := w.sends(); !slices.Equal(got, []string{"surface:2"}) {
		t.Errorf("送信先 %v, want surface:2 だけ", got)
	}
	state := loadState(h.env.StatePath)
	if _, ok := state.Attempts[a.uuid]; ok {
		t.Errorf("起動済みのタブの試行回数が残っている: %v", state.Attempts)
	}
	if !h.logged("スキップ surface:1 エージェントが起動済み") {
		t.Errorf("ログ %v", h.logs)
	}
}

func TestRestoreSkipsWhenPresenceUnknown(t *testing.T) {
	a := tabN(1, "workspace:1")
	w := newWorld(a)
	// 一覧作りの ps は通り、送る直前の確認で ps が失敗する
	w.onPsCall = func(w *world, n int) {
		if n >= 2 {
			w.psErr = errors.New("ps failed")
		}
	}
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.sends()) != 0 {
		t.Errorf("判定できないのに送った: %v", w.sends())
	}
	if state := loadState(h.env.StatePath); state.Attempts[a.uuid] != 0 {
		t.Errorf("試行回数に数えた: %v", state.Attempts)
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
		t.Errorf("送信先 %v", got)
	}
	if got := loadState(h.env.StatePath).Attempts[a.uuid]; got != 1 {
		t.Errorf("前面がシェルでないタブの試行回数が %d（1 のまま数えない）", got)
	}
	if !h.logged("スキップ surface:1 前面がシェルだけと確かめられない（busy）") {
		t.Errorf("ログ %v", h.logs)
	}
}

func TestRestoreSkipsWhenTtyPointsToOtherTab(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	w.ttyEnv[a.tty] = "vim CMUX_SURFACE_ID=4DE01DAC-A9A7-4308-939C-763E5880CFA0" // 古い tty
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := w.sends(); !slices.Equal(got, []string{"surface:2"}) {
		t.Errorf("送信先 %v", got)
	}
	if got := loadState(h.env.StatePath).Attempts[a.uuid]; got != 0 {
		t.Errorf("tty が別のタブを指すタブの試行回数が %d", got)
	}
	if !h.logged("スキップ surface:1 前面がシェルだけと確かめられない（unknown）") {
		t.Errorf("ログ %v", h.logs)
	}
}

func TestRestoreSkipsWhenNoTty(t *testing.T) {
	a := tabN(1, "workspace:1")
	a.tty = ""
	w := newWorld(a)
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.sends()) != 0 {
		t.Errorf("tty が無いのに送った")
	}
}

func TestRestoreSelectsByKind(t *testing.T) {
	a, b, c, d := tabN(1, "workspace:1"), tabN(2, "workspace:1"), tabN(3, "workspace:1"), tabN(4, "workspace:1")
	b.kind = "codex"
	c.kind = "gemini"
	d.kind = "" // 記録なし
	br := tabN(5, "workspace:1")
	br.browser = true
	w := newWorld(a, b, c, d, br)
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := w.sends(); !slices.Equal(got, []string{"surface:1", "surface:2"}) {
		t.Errorf("送信先 %v", got)
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
		t.Errorf("送信先 %v", got)
	}
}

func TestRestoreDoesNotReadRecordOfTabWithAgent(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:1")
	w := newWorld(a, b)
	w.agents[a.uuid] = true
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if slices.Contains(w.kindsRead, "surface:1") {
		t.Error("エージェントがいるタブの復元記録を読んだ")
	}
}

func TestRestoreDoesNothingWhenCmuxDoesNotRespond(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"))
	w.treeErr = errors.New("cmux が応答しない")
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if len(w.events) != 0 || w.psCalls != 0 || len(h.logs) != 0 {
		t.Errorf("何もしないはずが動いた: events=%v ps=%d logs=%v", w.events, w.psCalls, h.logs)
	}
}

func TestRestoreFailsWhenPsFails(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"))
	w.psErr = errors.New("ps failed")
	h := newHarness(t, w)
	if err := Restore(h.env, defaultOptions()); err == nil {
		t.Error("ps が失敗したのに error にならなかった")
	}
	if len(w.sends()) != 0 {
		t.Error("ps が失敗したのに送った")
	}
}

func TestRestoreCountsAttemptEvenWhenSendFails(t *testing.T) {
	a := tabN(1, "workspace:1")
	w := newWorld(a)
	w.sendError[a.ref] = errors.New("send failed")
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	if got := loadState(h.env.StatePath).Attempts[a.uuid]; got != 1 {
		t.Errorf("送信が失敗しても試行に数える（シェルに打ち込めた可能性があるため）: %d", got)
	}
	if !h.logged("WARN 送信 surface:1 失敗") {
		t.Errorf("ログ %v", h.logs)
	}
}

func TestRestoreClearsStatusInEveryWorkspace(t *testing.T) {
	a, b := tabN(1, "workspace:1"), tabN(2, "workspace:2")
	w := newWorld(a, b)
	h := newHarness(t, w)
	mustRestore(t, h, defaultOptions())
	for _, ws := range []string{"workspace:1", "workspace:2"} {
		if !slices.Contains(w.events, "clear-status "+ws) {
			t.Errorf("%s の表示を消していない: %v", ws, w.events)
		}
	}
}
