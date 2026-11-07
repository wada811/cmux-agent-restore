package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseTree(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"), tabN(2, "workspace:2"))
	br := tabN(3, "workspace:2")
	br.browser = true
	w.tabs = append(w.tabs, br)
	refs, tabs, err := parseTree(w.treeJSON())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refs, []string{"workspace:1", "workspace:2"}) {
		t.Errorf("workspace %v", refs)
	}
	if len(tabs) != 2 || tabs[1].WorkspaceRef != "workspace:2" || tabs[0].TTY != "ttys001" {
		t.Errorf("tabs %+v（terminal だけを拾う）", tabs)
	}
	if _, _, err := parseTree("not json"); err == nil {
		t.Error("壊れた JSON が error にならなかった")
	}
}

func TestParseRestoreKind(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"claude", `{"restore_record":{"checkpoint_id":"c1","kind":"claude"}}`, "claude"},
		{"checkpoint_id が無ければ記録なし", `{"restore_record":{"kind":"claude"}}`, ""},
		{"checkpoint_id が空なら記録なし", `{"restore_record":{"checkpoint_id":"","kind":"claude"}}`, ""},
		{"記録が null", `{"restore_record":null}`, ""},
		{"記録の項目が無い", `{}`, ""},
		{"kind が無ければ unknown", `{"restore_record":{"checkpoint_id":"c1"}}`, "unknown"},
		{"壊れた JSON", `oops`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseRestoreKind(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestFindCmux(t *testing.T) {
	found := func(string) (string, error) { return "/opt/homebrew/bin/cmux", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	statOK := func(string) (os.FileInfo, error) { return nil, nil }
	statNG := func(string) (os.FileInfo, error) { return nil, errors.New("no") }

	if p, err := findCmux(found, statNG); err != nil || p != "/opt/homebrew/bin/cmux" {
		t.Errorf("PATH から探す: %q %v", p, err)
	}
	if p, err := findCmux(missing, statOK); err != nil || p != bundledCmux {
		t.Errorf("PATH に無ければ同梱の CLI: %q %v", p, err)
	}
	if _, err := findCmux(missing, statNG); !errors.Is(err, errNoCmux) {
		t.Errorf("どちらも無ければ error: %v", err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	if s := loadState(p); len(s.Attempts) != 0 {
		t.Errorf("無いファイルは空の状態: %v", s)
	}
	if err := saveState(p, State{Attempts: map[string]int{"U": 2}, LastRun: "x"}); err != nil {
		t.Fatal(err)
	}
	if s := loadState(p); s.Attempts["U"] != 2 || s.LastRun != "x" {
		t.Errorf("読み戻せない: %v", s)
	}
	if err := os.WriteFile(p, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := loadState(p); s.Attempts == nil || len(s.Attempts) != 0 {
		t.Errorf("壊れたファイルは空の状態: %v", s)
	}
}

func TestLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	release, err := acquireLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(p); !errors.Is(err, errLocked) {
		t.Errorf("二重に取れた: %v", err)
	}
	release()
	release2, err := acquireLock(p)
	if err != nil {
		t.Fatalf("手放したのに取れない: %v", err)
	}
	release2()
}

func TestPlistXML(t *testing.T) {
	got := plistXML("/opt/homebrew/bin/cmux-agent-restore", []string{"--batch", "5"}, 5*time.Minute, "/Users/a&b", "andy")
	for _, want := range []string{
		"<string>com.wada811.cmux-agent-restore</string>",
		"<string>/opt/homebrew/bin/cmux-agent-restore</string>\n        <string>run</string>\n        <string>--batch</string>\n        <string>5</string>",
		"<key>StartInterval</key>\n    <integer>300</integer>",
		"<key>HOME</key>\n        <string>/Users/a&amp;b</string>",
		"<key>LOGNAME</key>\n        <string>andy</string>",
		"<key>RunAtLoad</key>\n    <false/>",
		"/Users/a&amp;b/Library/Logs/cmux-agent-restore.err.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist に %q が無い:\n%s", want, got)
		}
	}
}

func TestInstallAndUninstall(t *testing.T) {
	home := t.TempDir()
	p := defaultPaths(home)
	var calls []string
	run := func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}
	var out bytes.Buffer
	if err := installAgent(run, p, "/bin/car", []string{"--kinds", "claude"}, 10*time.Minute, 501, "andy", &out); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"launchctl bootout gui/501/com.wada811.cmux-agent-restore",
		"launchctl bootstrap gui/501 " + p.plistFile,
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("launchctl %v, want %v", calls, wantCalls)
	}
	fi, err := os.Lstat(p.plistFile)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("plist が実ファイルで書かれていない: %v", err)
	}
	b, _ := os.ReadFile(p.plistFile)
	if !strings.Contains(string(b), "<integer>600</integer>") || !strings.Contains(string(b), "<string>claude</string>") {
		t.Errorf("plist %s", b)
	}

	calls = nil
	if err := uninstallAgent(run, p, 501, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"launchctl bootout gui/501/com.wada811.cmux-agent-restore"}) {
		t.Errorf("launchctl %v", calls)
	}
	if _, err := os.Stat(p.plistFile); !os.IsNotExist(err) {
		t.Error("plist が残っている")
	}
	// 未登録・plist なしでも失敗しない
	failing := func(string, ...string) (string, error) { return "not loaded", errors.New("exit 3") }
	if err := uninstallAgent(failing, p, 501, &out); err != nil {
		t.Errorf("未登録の uninstall が失敗した: %v", err)
	}
}

func TestInstallFailsWhenBootstrapFails(t *testing.T) {
	p := defaultPaths(t.TempDir())
	run := func(name string, args ...string) (string, error) {
		if args[0] == "bootstrap" {
			return "Bootstrap failed: 5", errors.New("exit 5")
		}
		return "", nil
	}
	if err := installAgent(run, p, "/bin/car", nil, 5*time.Minute, 501, "andy", &bytes.Buffer{}); err == nil {
		t.Error("bootstrap の失敗が error にならなかった")
	}
}

func TestPrintStatus(t *testing.T) {
	p := defaultPaths(t.TempDir())
	_ = saveState(p.stateFile, State{Attempts: map[string]int{"U-A": 3, "U-B": 1}, LastRun: "2026-11-07T17:00:00Z"})
	run := func(name string, args ...string) (string, error) {
		return "gui/501/com.wada811.cmux-agent-restore = {\n\tstate = not running\n\truns = 4\n\tlast exit code = 0\n\trun interval = 300 seconds\n}", nil
	}
	var out bytes.Buffer
	printStatus(run, p, 501, 3, &out)
	for _, want := range []string{"state=not running", "実行回数=4", "最後の実行: 2026-11-07T17:00:00Z", "U-A 3/3（打ち切り", "U-B 1/3\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status に %q が無い:\n%s", want, out.String())
		}
	}
	out.Reset()
	printStatus(func(string, ...string) (string, error) { return "", errors.New("x") }, defaultPaths(t.TempDir()), 501, 3, &out)
	if !strings.Contains(out.String(), "launchd: 未登録") || !strings.Contains(out.String(), "state.json: なし") {
		t.Errorf("未登録・状態なし:\n%s", out.String())
	}
}

func testDeps(t *testing.T) deps {
	return deps{
		run:      func(string, ...string) (string, error) { return "", nil },
		paths:    defaultPaths(t.TempDir()),
		lookPath: func(string) (string, error) { return "", errors.New("none") },
		stat:     func(string) (os.FileInfo, error) { return nil, errors.New("none") },
		sleep:    func(time.Duration) {},
		now:      func() time.Time { return time.Date(2026, 11, 7, 17, 0, 0, 0, time.UTC) },
		exe:      func() (string, error) { return "/bin/car", nil },
		uid:      501,
		user:     "andy",
	}
}

func TestExecute(t *testing.T) {
	d := testDeps(t)
	run := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := execute(args, &o, &e, d)
		return code, o.String(), e.String()
	}
	if code, out, _ := run("--version"); code != 0 || !strings.HasPrefix(out, "cmux-agent-restore ") {
		t.Errorf("--version: %d %q", code, out)
	}
	if code, out, _ := run("--help"); code != 0 || !strings.Contains(out, "サブコマンド") {
		t.Errorf("--help: %d", code)
	}
	if code, _, _ := run(); code != 2 {
		t.Errorf("引数なし: %d", code)
	}
	if code, _, errOut := run("frobnicate"); code != 2 || !strings.Contains(errOut, "frobnicate") {
		t.Errorf("不明なサブコマンド: %d %q", code, errOut)
	}
	for _, bad := range [][]string{
		{"run", "--batch", "0"}, {"run", "--max-attempts", "0"}, {"run", "--kinds", ","},
		{"run", "extra"}, {"run", "--nope"}, {"install", "--interval", "10s"}, {"uninstall", "x"},
	} {
		if code, _, _ := run(bad...); code != 2 {
			t.Errorf("%v: %d, want 2", bad, code)
		}
	}
	if code, _, errOut := run("run", "--dry-run"); code != 1 || !strings.Contains(errOut, "cmux が見つからない") {
		t.Errorf("cmux が無い: %d %q", code, errOut)
	}
}

func TestExecuteRunDryRun(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"))
	d := testDeps(t)
	d.run = w.run
	d.lookPath = func(string) (string, error) { return "/fake/cmux", nil }
	var o, e bytes.Buffer
	if code := execute([]string{"run", "--dry-run"}, &o, &e, d); code != 0 {
		t.Fatalf("code %d: %s", code, e.String())
	}
	if !strings.Contains(o.String(), "[dry-run] 対象 1 件（送信 1・戻らない 0）") {
		t.Errorf("出力:\n%s", o.String())
	}
	if _, err := os.Stat(d.paths.logFile); !os.IsNotExist(err) {
		t.Error("dry-run がログファイルを作った")
	}
	if len(w.events) != 0 {
		t.Errorf("dry-run で書いた: %v", w.events)
	}
}

func TestExecuteRunLocked(t *testing.T) {
	d := testDeps(t)
	d.lookPath = func(string) (string, error) { return "/fake/cmux", nil }
	d.run = func(string, ...string) (string, error) {
		t.Error("ロック中なのに cmux を呼んだ")
		return "", nil
	}
	release, err := acquireLock(d.paths.lockFile)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var o, e bytes.Buffer
	if code := execute([]string{"run"}, &o, &e, d); code != 0 || !strings.Contains(o.String(), "別のプロセスが実行中") {
		t.Errorf("code %d out %q", code, o.String())
	}
}

func TestExecuteRunWritesLog(t *testing.T) {
	w := newWorld(tabN(1, "workspace:1"))
	d := testDeps(t)
	d.run = w.run
	d.lookPath = func(string) (string, error) { return "/fake/cmux", nil }
	var o, e bytes.Buffer
	if code := execute([]string{"run"}, &o, &e, d); code != 0 {
		t.Fatalf("code %d: %s", code, e.String())
	}
	b, err := os.ReadFile(d.paths.logFile)
	if err != nil || !strings.Contains(string(b), "送信 surface:1 1/3") {
		t.Errorf("ログファイル: %v\n%s", err, b)
	}
	if s := loadState(d.paths.stateFile); s.Attempts[tabN(1, "").uuid] != 1 {
		t.Errorf("state %v", s)
	}
}

func TestInstallPassesOnlyExplicitFlags(t *testing.T) {
	d := testDeps(t)
	var calls []string
	d.run = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}
	d.paths = defaultPaths(t.TempDir())
	var o, e bytes.Buffer
	if code := execute([]string{"install", "--interval", "10m", "--kinds", "claude"}, &o, &e, d); code != 0 {
		t.Fatalf("code %d: %s", code, e.String())
	}
	b, _ := os.ReadFile(d.paths.plistFile)
	s := string(b)
	if !strings.Contains(s, "<string>--kinds</string>\n        <string>claude</string>") || strings.Contains(s, "--batch") {
		t.Errorf("plist の引数:\n%s", s)
	}
	if !strings.Contains(s, "<integer>600</integer>") {
		t.Errorf("間隔:\n%s", s)
	}
}
