package main

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Runner は外部コマンド（cmux・ps・launchctl）を 1 回実行して標準出力を返す。
// 試験では差し替える。コマンドが失敗しても、出力が取れていれば出力と error の両方を返す
// （ps -p は途中で終了した pid があると exit 1 になるが、出力は有効なため）。
type Runner func(name string, args ...string) (string, error)

// Presence はあるタブにエージェントがいるかの判定結果。
type Presence string

const (
	Present Presence = "present"
	Absent  Presence = "absent"
	Unknown Presence = "unknown"
)

// Foreground はタブの tty の前面がシェルだけかの判定結果。
type Foreground string

const (
	FgShell   Foreground = "shell"
	FgBusy    Foreground = "busy"
	FgUnknown Foreground = "unknown"
)

// shells は前面にいてよいプロセスの名前。これ以外が前面なら送らない。
var shells = []string{"zsh", "bash", "sh", "dash", "fish"}

// DefaultKinds は cmux restore を送る対象にする restore_record の kind。
var DefaultKinds = []string{"claude", "codex"}

func basename(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// AgentProcess は ps から拾ったエージェントの pid と種類。
type AgentProcess struct {
	PID  string
	Kind string
}

var psLineRe = regexp.MustCompile(`^(\d+)\s+(.*)$`)

// agentKind は `ps -axo pid=,command=` の 1 行のコマンド部分から、エージェントの種類を返す。
// エージェントでなければ空文字。
//
// 実体 = 実行ファイルの名前（basename）が claude か codex（node 経由なら node の次の語）。
// codex の常駐プロセス（`codex app-server …`・app-server-daemon 配下）と codex-code-mode-host は除く。
// 常駐プロセスは古い CMUX_SURFACE_ID を持っているので、拾うと誤判定する。
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

// parseAgentProcesses は `ps -axo pid=,command=` の出力から、エージェントの pid と種類を拾う。
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

// parseSurfaceIDs は `ps eww` の出力から CMUX_SURFACE_ID の UUID（大文字）を集める。
func parseSurfaceIDs(psEwwOutput string) map[string]bool {
	found := map[string]bool{}
	for _, m := range surfaceIDRe.FindAllStringSubmatch(psEwwOutput, -1) {
		found[strings.ToUpper(m[1])] = true
	}
	return found
}

// surfacesWithAgent は、エージェント（claude・codex のどちらでも）が環境変数 CMUX_SURFACE_ID に
// 持つ surface UUID（大文字）の集合を返す。ps 自体が失敗したら error。
func surfacesWithAgent(run Runner) (map[string]bool, error) {
	out, err := run("ps", "-axo", "pid=,command=")
	if err != nil {
		// 一覧が欠けているかもしれないので、出力があっても失敗にする。
		// 「いない」と誤って判定すると、動いているエージェントのタブに送ってしまう
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
	// 途中で終了した pid があると exit 1 になるが出力は有効なので、error は見ない
	envOut, _ := run("ps", "eww", "-p", strings.Join(pids, ","), "-o", "command=")
	return parseSurfaceIDs(envOut), nil
}

// checkAgentOnSurface は surface にエージェントがいるかを判定する。
// いないと決めるのは、間隔を置いた 2 回とも見えなかったときだけ。
// 判定できない場合（surfaceID が無い・ps が失敗）は Unknown で、呼び出し側は「いる」側に倒す。
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

// selectTargets は復元を送る対象を選ぶ。
// 対象 = restore_record の kind が kinds に含まれ、その surface にエージェントが（種類を問わず）いないタブ。
// kindOf が空文字を返す（記録が無い・読めない）タブ、kind が不明なタブは対象にしない。
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

// classifyForeground は `ps -t <tty> -o pid=,stat=,comm=` の出力から、その tty の前面
// （stat に + が付く）がシェルだけかを判定する。前面のプロセスが 1 つも見えなければ FgUnknown。
// 環境変数は読まない（cmux 0.65.2 はシェルを root 所有の login の子として起動し、
// シェルの環境変数が読めないため）。
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

// ttyHasOtherSurface は `ps eww -t <tty> -o command=` の出力に、別の surface の
// CMUX_SURFACE_ID を持つプロセスがあるかを返す。あれば `cmux tree` の tty が古く、別のタブを指している。
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

// foregroundOnTty は tty（`cmux tree` の surface.tty。例 ttys117）の前面がシェルだけかを実際の ps で確かめる。
// tty が無い・ps が失敗した・その tty に別のタブのプロセスがいるときは FgUnknown。
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

var errNoCmux = errors.New("cmux が見つからない")

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
