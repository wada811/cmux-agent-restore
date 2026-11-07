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
	t.Run("picks up claude and codex processes with their kind", func(t *testing.T) {
		want := []AgentProcess{{"202", "claude"}, {"303", "codex"}, {"304", "codex"}}
		if got := parseAgentProcesses(psSample); !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("ignores resident processes, codex-code-mode-host, grep and shell lines", func(t *testing.T) {
		var pids []string
		for _, p := range parseAgentProcesses(psSample) {
			pids = append(pids, p.PID)
		}
		for _, ignored := range []string{"305", "306", "307", "308", "101"} {
			if slices.Contains(pids, ignored) {
				t.Errorf("picked up %s", ignored)
			}
		}
	})
	t.Run("empty in, empty out", func(t *testing.T) {
		if got := parseAgentProcesses(""); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("claude is matched by executable name (the path does not matter)", func(t *testing.T) {
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
	t.Run("a codex record on a tab where Codex is running is not a target", func(t *testing.T) {
		if got := pick([]string{U1}, map[string]string{U1: "codex"}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("a tab with a running agent is not a target whatever the record kind", func(t *testing.T) {
		if got := pick([]string{U1, U2}, map[string]string{U1: "claude", U2: "codex"}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("claude and codex records on tabs with no agent are targets", func(t *testing.T) {
		got := pick(nil, map[string]string{U1: "claude", U2: "codex"})
		if !reflect.DeepEqual(got, []string{U1, U2}) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("tabs with an unknown kind or no record are not targets", func(t *testing.T) {
		if got := pick(nil, map[string]string{U1: "unknown", U2: "gemini", U3: ""}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("surface UUIDs are matched ignoring case", func(t *testing.T) {
		lower := []Tab{{UUID: strings.ToLower(U1)}}
		got := selectTargets(lower, map[string]bool{U1: true}, func(Tab) string { return "codex" }, DefaultKinds)
		if len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("--kinds changes the targeted kinds", func(t *testing.T) {
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
		{"only zsh in front is shell (login shell)", line("1", "Ss+", "-/bin/zsh"), FgShell},
		{"only zsh in front is shell", line("1", "S+", "-zsh"), FgShell},
		{"claude in front is busy (do not send)", line("1", "Ss", "-/bin/zsh") + "\n" + line("2", "S+", "/Users/andy/.local/bin/claude"), FgBusy},
		{"an editor in front is busy", line("1", "Ss", "-/bin/zsh") + "\n" + line("2", "S+", "vim"), FgBusy},
		{"no foreground is unknown", line("1", "Ss", "-/bin/zsh"), FgUnknown},
		{"empty is unknown", "", FgUnknown},
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
	t.Run("false if no process has a readable environment", func(t *testing.T) {
		if ttyHasOtherSurface("-/bin/zsh\n", id) {
			t.Error("true")
		}
	})
	t.Run("false if only the same surface ID appears (case does not matter)", func(t *testing.T) {
		if ttyHasOtherSurface("claude CMUX_SURFACE_ID="+strings.ToUpper(id)+" HOME=/x", id) {
			t.Error("true")
		}
	})
	t.Run("true if another surface's ID appears (the tty is stale)", func(t *testing.T) {
		if !ttyHasOtherSurface("vim CMUX_SURFACE_ID=4DE01DAC-A9A7-4308-939C-763E5880CFA0", id) {
			t.Error("false")
		}
	})
}

func TestForegroundOnTty(t *testing.T) {
	const id = "5FE32830-90E8-4C23-9798-DEDEE480C2D1"
	never := func(name string, args ...string) (string, error) {
		t.Fatalf("ps was called: %v", args)
		return "", nil
	}
	t.Run("no tty is unknown (ps is not called)", func(t *testing.T) {
		if got := foregroundOnTty(never, "", id); got != FgUnknown {
			t.Errorf("got %v", got)
		}
	})
	t.Run("a tty with unusable characters is unknown (ps is not called)", func(t *testing.T) {
		if got := foregroundOnTty(never, "ttys1; rm", id); got != FgUnknown {
			t.Errorf("got %v", got)
		}
	})
	t.Run("a tty that does not exist (ps fails) is unknown", func(t *testing.T) {
		fail := func(string, ...string) (string, error) { return "", errors.New("exit 1") }
		if got := foregroundOnTty(fail, "ttys99999", id); got != FgUnknown {
			t.Errorf("got %v", got)
		}
	})
	t.Run("only a shell in front is shell", func(t *testing.T) {
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
	t.Run("present if the probe sees it (stops after one call)", func(t *testing.T) {
		n := 0
		probe := func() (map[string]bool, error) { n++; return set(U1), nil }
		if got := checkAgentOnSurface(U1, probe, noSleep, 0); got != Present || n != 1 {
			t.Errorf("got %v, %d calls", got, n)
		}
	})
	t.Run("present if the second look sees it even if the first did not", func(t *testing.T) {
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
	t.Run("absent if neither look sees it; pause between looks", func(t *testing.T) {
		var slept []time.Duration
		probe := func() (map[string]bool, error) { return set(), nil }
		got := checkAgentOnSurface(U1, probe, func(d time.Duration) { slept = append(slept, d) }, 1500*time.Millisecond)
		if got != Absent || !reflect.DeepEqual(slept, []time.Duration{1500 * time.Millisecond}) {
			t.Errorf("got %v, slept %v", got, slept)
		}
	})
	t.Run("UUIDs are compared ignoring case", func(t *testing.T) {
		probe := func() (map[string]bool, error) { return set(U1), nil }
		if got := checkAgentOnSurface(strings.ToLower(U1), probe, noSleep, 0); got != Present {
			t.Errorf("got %v", got)
		}
	})
	t.Run("no surfaceID is unknown", func(t *testing.T) {
		if got := checkAgentOnSurface("", func() (map[string]bool, error) { return set(), nil }, noSleep, 0); got != Unknown {
			t.Errorf("got %v", got)
		}
	})
	t.Run("unknown if the probe fails", func(t *testing.T) {
		probe := func() (map[string]bool, error) { return nil, errors.New("ps failed") }
		if got := checkAgentOnSurface(U1, probe, noSleep, 0); got != Unknown {
			t.Errorf("got %v", got)
		}
	})
}

func TestSurfacesWithAgent(t *testing.T) {
	t.Run("uses the output even if ps eww exits 1", func(t *testing.T) {
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
	t.Run("does not call ps eww when no agent is running", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			if slices.Contains(args, "eww") {
				t.Error("ps eww was called")
			}
			return "101 /bin/zsh\n", nil
		}
		got, err := surfacesWithAgent(run)
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("error even if the first ps fails with output (the list may be incomplete)", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			return "202 /Users/andy/.local/bin/claude\n", errors.New("exit 1")
		}
		if _, err := surfacesWithAgent(run); err == nil {
			t.Error("no error was returned")
		}
	})
	t.Run("error if ps fails", func(t *testing.T) {
		run := func(string, ...string) (string, error) { return "", errors.New("boom") }
		if _, err := surfacesWithAgent(run); err == nil {
			t.Error("no error was returned")
		}
	})
}
