package main

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	U1 = "0A1B2C3D-0000-4000-8000-ABCDEF012345"
	U2 = "1A1B2C3D-0000-4000-8000-ABCDEF012345"
	U3 = "2A1B2C3D-0000-4000-8000-ABCDEF012345"
	U4 = "3A1B2C3D-0000-4000-8000-ABCDEF012345"
)

const mise = "/Users/andy/.local/share/mise/installs/node"

var psSample = strings.Join([]string{
	"  101 /bin/zsh -l",
	"  202 /Users/andy/.local/bin/claude --resume x",
	"  303 node " + mise + "/26/bin/codex resume abc",
	"  304 " + mise + "/26.12.1/lib/node_modules/@openai/codex/node_modules/@openai/codex-darwin-arm64/vendor/aarch64-apple-darwin/bin/codex --disable computer_use",
	"  305 /Users/andy/.codex/packages/app-server-daemon/releases/0.167.0-aarch64-apple-darwin/bin/codex app-server --listen unix://",
	"  306 /Users/andy/.codex/packages/app-server-daemon/releases/0.167.0-aarch64-apple-darwin/bin/codex app-server daemon pid-update-loop",
	"  307 " + mise + "/26.12.1/lib/node_modules/@openai/codex/node_modules/@openai/codex-darwin-arm64/vendor/aarch64-apple-darwin/bin/codex-code-mode-host",
	"  308 grep codex",
}, "\n")

func TestParseAgentProcesses(t *testing.T) {
	t.Run("claude と codex の実体を種類つきで拾う", func(t *testing.T) {
		want := []AgentProcess{{"202", "claude"}, {"303", "codex"}, {"304", "codex"}}
		if got := parseAgentProcesses(psSample); !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("常駐プロセス・codex-code-mode-host・grep・シェルの行は無視する", func(t *testing.T) {
		var pids []string
		for _, p := range parseAgentProcesses(psSample) {
			pids = append(pids, p.PID)
		}
		for _, ignored := range []string{"305", "306", "307", "308", "101"} {
			if slices.Contains(pids, ignored) {
				t.Errorf("%s を拾った", ignored)
			}
		}
	})
	t.Run("空なら空", func(t *testing.T) {
		if got := parseAgentProcesses(""); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("claude は実行ファイル名で見る（パスに依らない）", func(t *testing.T) {
		out := strings.Join([]string{
			"  1 claude --resume x",
			"  2 /opt/homebrew/bin/claude",
			"  3 node /x/node_modules/.bin/claude",
			"  4 /Users/andy/.local/bin/claudette",
			"  5 vim /Users/andy/.local/bin/claude",
			"  6 /Users/andy/.claude/hooks/run.sh",
		}, "\n")
		var pids []string
		for _, p := range parseAgentProcesses(out) {
			pids = append(pids, p.PID)
		}
		if want := []string{"1", "2", "3"}; !reflect.DeepEqual(pids, want) {
			t.Errorf("got %v, want %v", pids, want)
		}
	})
}

func TestParseSurfaceIDs(t *testing.T) {
	got := parseSurfaceIDs("claude CMUX_SURFACE_ID=" + strings.ToLower(U1) + " HOME=/x\nnode CMUX_SURFACE_ID=" + U2 + "\nCMUX_SURFACE_ID=short")
	if !got[U1] || !got[U2] || len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

func TestSelectTargets(t *testing.T) {
	tabs := []Tab{{UUID: U1}, {UUID: U2}, {UUID: U3}, {UUID: U4}}
	pick := func(withAgent []string, kinds map[string]string) []string {
		set := map[string]bool{}
		for _, u := range withAgent {
			set[u] = true
		}
		var got []string
		for _, t := range selectTargets(tabs, set, func(t Tab) string { return kinds[t.UUID] }, DefaultKinds) {
			got = append(got, t.UUID)
		}
		return got
	}
	t.Run("Codex が動いているタブの codex 記録は対象外", func(t *testing.T) {
		if got := pick([]string{U1}, map[string]string{U1: "codex"}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("エージェントが動いているタブは、記録の kind が何でも対象外", func(t *testing.T) {
		if got := pick([]string{U1, U2}, map[string]string{U1: "claude", U2: "codex"}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("エージェントがいないタブの claude・codex 記録は対象", func(t *testing.T) {
		got := pick(nil, map[string]string{U1: "claude", U2: "codex"})
		if !reflect.DeepEqual(got, []string{U1, U2}) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("kind 不明・記録なしのタブは対象外", func(t *testing.T) {
		if got := pick(nil, map[string]string{U1: "unknown", U2: "gemini", U3: ""}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("surface の UUID は大文字小文字を区別せず突き合わせる", func(t *testing.T) {
		lower := []Tab{{UUID: strings.ToLower(U1)}}
		got := selectTargets(lower, map[string]bool{U1: true}, func(Tab) string { return "codex" }, DefaultKinds)
		if len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("--kinds で対象の種類を変えられる", func(t *testing.T) {
		got := selectTargets(tabs[:2], map[string]bool{}, func(t Tab) string {
			return map[string]string{U1: "claude", U2: "opencode"}[t.UUID]
		}, []string{"opencode"})
		if len(got) != 1 || got[0].UUID != U2 {
			t.Errorf("got %v", got)
		}
	})
}

func TestClassifyForeground(t *testing.T) {
	line := func(pid, stat, comm string) string { return pid + " " + stat + " " + comm }
	cases := []struct {
		name string
		out  string
		want Foreground
	}{
		{"前面が zsh だけなら shell (login シェル)", line("1", "Ss+", "-/bin/zsh"), FgShell},
		{"前面が zsh だけなら shell", line("1", "S+", "-zsh"), FgShell},
		{"claude が前面なら busy（送らない）", line("1", "Ss", "-/bin/zsh") + "\n" + line("2", "S+", "/Users/andy/.local/bin/claude"), FgBusy},
		{"エディタが前面なら busy", line("1", "Ss", "-/bin/zsh") + "\n" + line("2", "S+", "vim"), FgBusy},
		{"前面がなければ unknown", line("1", "Ss", "-/bin/zsh"), FgUnknown},
		{"空は unknown", "", FgUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyForeground(c.out); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestTtyHasOtherSurface(t *testing.T) {
	const id = "5fe32830-90e8-4c23-9798-dedee480c2d1"
	t.Run("環境変数が読めるプロセスが無ければ false", func(t *testing.T) {
		if ttyHasOtherSurface("-/bin/zsh\n", id) {
			t.Error("true")
		}
	})
	t.Run("同じ surface の ID だけなら false（大文字小文字は問わない）", func(t *testing.T) {
		if ttyHasOtherSurface("claude CMUX_SURFACE_ID="+strings.ToUpper(id)+" HOME=/x", id) {
			t.Error("true")
		}
	})
	t.Run("別の surface の ID があれば true（tty が古い）", func(t *testing.T) {
		if !ttyHasOtherSurface("vim CMUX_SURFACE_ID=4DE01DAC-A9A7-4308-939C-763E5880CFA0", id) {
			t.Error("false")
		}
	})
}

func TestForegroundOnTty(t *testing.T) {
	const id = "5FE32830-90E8-4C23-9798-DEDEE480C2D1"
	never := func(name string, args ...string) (string, error) {
		t.Fatalf("ps を呼んだ: %v", args)
		return "", nil
	}
	t.Run("tty が無ければ unknown（ps を呼ばない）", func(t *testing.T) {
		if got := foregroundOnTty(never, "", id); got != FgUnknown {
			t.Errorf("got %v", got)
		}
	})
	t.Run("tty に使えない文字があれば unknown（ps を呼ばない）", func(t *testing.T) {
		if got := foregroundOnTty(never, "ttys1; rm", id); got != FgUnknown {
			t.Errorf("got %v", got)
		}
	})
	t.Run("存在しない tty（ps が失敗）は unknown", func(t *testing.T) {
		fail := func(string, ...string) (string, error) { return "", errors.New("exit 1") }
		if got := foregroundOnTty(fail, "ttys99999", id); got != FgUnknown {
			t.Errorf("got %v", got)
		}
	})
	t.Run("前面がシェルだけなら shell", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			if slices.Contains(args, "eww") {
				return "-zsh\n", nil
			}
			return "1 Ss+ -/bin/zsh\n", nil
		}
		if got := foregroundOnTty(run, "ttys1", id); got != FgShell {
			t.Errorf("got %v", got)
		}
	})
}

func TestCheckAgentOnSurface(t *testing.T) {
	noSleep := func(time.Duration) {}
	set := func(ids ...string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	t.Run("プローブに見えれば present（1 回で打ち切る）", func(t *testing.T) {
		n := 0
		probe := func() (map[string]bool, error) { n++; return set(U1), nil }
		if got := checkAgentOnSurface(U1, probe, noSleep, 0); got != Present || n != 1 {
			t.Errorf("got %v, 呼び出し %d 回", got, n)
		}
	})
	t.Run("1 回目に見えなくても 2 回目に見えれば present", func(t *testing.T) {
		n := 0
		probe := func() (map[string]bool, error) {
			n++
			if n == 2 {
				return set(U1), nil
			}
			return set(), nil
		}
		if got := checkAgentOnSurface(U1, probe, noSleep, 0); got != Present {
			t.Errorf("got %v", got)
		}
	})
	t.Run("2 回とも見えなければ absent。間隔をあける", func(t *testing.T) {
		var slept []time.Duration
		probe := func() (map[string]bool, error) { return set(), nil }
		got := checkAgentOnSurface(U1, probe, func(d time.Duration) { slept = append(slept, d) }, 1500*time.Millisecond)
		if got != Absent || !reflect.DeepEqual(slept, []time.Duration{1500 * time.Millisecond}) {
			t.Errorf("got %v, slept %v", got, slept)
		}
	})
	t.Run("UUID は大文字小文字を区別しない", func(t *testing.T) {
		probe := func() (map[string]bool, error) { return set(U1), nil }
		if got := checkAgentOnSurface(strings.ToLower(U1), probe, noSleep, 0); got != Present {
			t.Errorf("got %v", got)
		}
	})
	t.Run("surfaceID が無ければ unknown", func(t *testing.T) {
		if got := checkAgentOnSurface("", func() (map[string]bool, error) { return set(), nil }, noSleep, 0); got != Unknown {
			t.Errorf("got %v", got)
		}
	})
	t.Run("プローブが失敗したら unknown", func(t *testing.T) {
		probe := func() (map[string]bool, error) { return nil, errors.New("ps failed") }
		if got := checkAgentOnSurface(U1, probe, noSleep, 0); got != Unknown {
			t.Errorf("got %v", got)
		}
	})
}

func TestSurfacesWithAgent(t *testing.T) {
	t.Run("ps eww が exit 1 でも出力を使う", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			if slices.Contains(args, "eww") {
				return "claude CMUX_SURFACE_ID=" + U1, errors.New("exit 1")
			}
			return "202 /Users/andy/.local/bin/claude\n", nil
		}
		got, err := surfacesWithAgent(run)
		if err != nil || !got[U1] {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("エージェントがいなければ ps eww を呼ばない", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			if slices.Contains(args, "eww") {
				t.Error("ps eww を呼んだ")
			}
			return "101 /bin/zsh\n", nil
		}
		got, err := surfacesWithAgent(run)
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("最初の ps が出力つきで失敗しても error（一覧が欠けているかもしれない）", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			return "202 /Users/andy/.local/bin/claude\n", errors.New("exit 1")
		}
		if _, err := surfacesWithAgent(run); err == nil {
			t.Error("error にならなかった")
		}
	})
	t.Run("ps が失敗したら error", func(t *testing.T) {
		run := func(string, ...string) (string, error) { return "", errors.New("boom") }
		if _, err := surfacesWithAgent(run); err == nil {
			t.Error("error にならなかった")
		}
	})
}
