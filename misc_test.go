package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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
			got, err := parseRestoreKind(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if (err != nil) != (c.in == "oops") {
				t.Errorf("err = %v（読めない出力のときだけ error にする）", err)
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
	if err := saveState(p, State{Attempts: map[string]int{"U": 2}, LastCompletedRun: "x"}); err != nil {
		t.Fatal(err)
	}
	if s := loadState(p); s.Attempts["U"] != 2 || s.LastCompletedRun != "x" {
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

// fakeLaunchctl is launchctl with the timing that matters: bootout returns before the job is gone
// (print keeps succeeding for printAfterBootout more calls), and bootstrap can fail.
type fakeLaunchctl struct {
	loaded            bool
	printAfterBootout int // how many print calls still succeed after bootout
	bootstrapFails    int // how many bootstrap calls fail before one succeeds (-1: always)
	pending           int
	bootoutErr        error
	bootoutOut        string
	calls             []string
	bootstrapped      []string // plist content at each successful bootstrap
}

func (f *fakeLaunchctl) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "bootout":
		if f.bootoutErr != nil {
			return f.bootoutOut, f.bootoutErr
		}
		if !f.loaded {
			return "Boot-out failed: 3: No such process", errors.New("exit 3")
		}
		f.loaded = false
		f.pending = f.printAfterBootout
		return "", nil
	case "print":
		if f.loaded || f.pending > 0 {
			if f.pending > 0 {
				f.pending--
			}
			return "state = running", nil
		}
		return "", errors.New("exit 113")
	case "bootstrap":
		if f.bootstrapFails != 0 {
			if f.bootstrapFails > 0 {
				f.bootstrapFails--
			}
			return "", errors.New("Bootstrap failed: 5: Input/output error")
		}
		b, _ := os.ReadFile(args[2])
		f.bootstrapped = append(f.bootstrapped, string(b))
		f.loaded = true
		return "", nil
	}
	return "", nil
}

func noSleep(time.Duration) {}

func TestInstallFresh(t *testing.T) {
	p := defaultPaths(t.TempDir())
	f := &fakeLaunchctl{}
	var out bytes.Buffer
	if err := installAgent(f.run, noSleep, p, "/bin/car", []string{"--kinds", "claude"}, 10*time.Minute, 501, "andy", &out); err != nil {
		t.Fatal(err)
	}
	if want := []string{"bootout", "print", "bootstrap"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("launchctl %v, want %v", f.calls, want)
	}
	fi, err := os.Lstat(p.plistFile)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("plist が実ファイルで書かれていない: %v", err)
	}
	b, _ := os.ReadFile(p.plistFile)
	if !strings.Contains(string(b), "<integer>600</integer>") || !strings.Contains(string(b), "<string>claude</string>") {
		t.Errorf("plist %s", b)
	}
	if _, err := os.Stat(p.plistFile + ".tmp"); !os.IsNotExist(err) {
		t.Error("一時ファイルが残っている")
	}
}

func TestWriteFileAtomicReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.plist")
	link := filepath.Join(dir, "link.plist")
	_ = os.WriteFile(real, []byte("old"), 0o644)
	_ = os.Symlink(real, link)
	if err := writeFileAtomic(link, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(link)
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("symlink のまま")
	}
	if b, _ := os.ReadFile(real); string(b) != "old" {
		t.Errorf("symlink の先を書き換えた: %q", b)
	}
}

func TestInstallWaitsForBootoutToFinish(t *testing.T) {
	p := defaultPaths(t.TempDir())
	f := &fakeLaunchctl{loaded: true, printAfterBootout: 3}
	var slept int
	sleep := func(time.Duration) { slept++ }
	if err := installAgent(f.run, sleep, p, "/bin/car", nil, 5*time.Minute, 501, "andy", &bytes.Buffer{}); err != nil {
		t.Fatalf("bootout が遅れても成功するはず: %v", err)
	}
	if slept < 3 {
		t.Errorf("print が失敗するまで待っていない（待ち %d 回）", slept)
	}
	if !f.loaded {
		t.Error("読み込まれていない")
	}
}

func TestInstallRetriesBootstrap(t *testing.T) {
	p := defaultPaths(t.TempDir())
	f := &fakeLaunchctl{bootstrapFails: 2}
	if err := installAgent(f.run, noSleep, p, "/bin/car", nil, 5*time.Minute, 501, "andy", &bytes.Buffer{}); err != nil {
		t.Fatalf("2 回失敗しても 3 回目で通るはず: %v", err)
	}
	n := 0
	for _, c := range f.calls {
		if c == "bootstrap" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("bootstrap %d 回, want 3", n)
	}
}

func TestInstallRestoresPreviousPlistWhenBootstrapFails(t *testing.T) {
	p := defaultPaths(t.TempDir())
	_ = os.MkdirAll(filepath.Dir(p.plistFile), 0o755)
	_ = os.WriteFile(p.plistFile, []byte("OLD PLIST"), 0o644)
	f := &fakeLaunchctl{loaded: true, bootstrapFails: bootstrapTries} // 新しい plist の bootstrap だけ失敗し、元のは通る
	err := installAgent(f.run, noSleep, p, "/bin/car", nil, 5*time.Minute, 501, "andy", &bytes.Buffer{})
	if err == nil {
		t.Fatal("bootstrap の失敗が error にならなかった")
	}
	if b, _ := os.ReadFile(p.plistFile); string(b) != "OLD PLIST" {
		t.Errorf("元の plist が戻っていない: %q", b)
	}
	if !f.loaded || len(f.bootstrapped) != 1 || f.bootstrapped[0] != "OLD PLIST" {
		t.Errorf("元の plist を読み込み直していない: loaded=%v bootstrapped=%q", f.loaded, f.bootstrapped)
	}
	if !strings.Contains(err.Error(), "previous plist was put back") {
		t.Errorf("error %v", err)
	}
}

func TestInstallRemovesNewPlistWhenThereWasNoPreviousAndBootstrapFails(t *testing.T) {
	p := defaultPaths(t.TempDir())
	f := &fakeLaunchctl{bootstrapFails: -1}
	if err := installAgent(f.run, noSleep, p, "/bin/car", nil, 5*time.Minute, 501, "andy", &bytes.Buffer{}); err == nil {
		t.Fatal("error にならなかった")
	}
	if _, err := os.Stat(p.plistFile); !os.IsNotExist(err) {
		t.Error("失敗した plist が残っている")
	}
}

func TestInstallFailsWhenJobStaysRegistered(t *testing.T) {
	p := defaultPaths(t.TempDir())
	f := &fakeLaunchctl{loaded: true, printAfterBootout: 1000}
	if err := installAgent(f.run, noSleep, p, "/bin/car", nil, 5*time.Minute, 501, "andy", &bytes.Buffer{}); err == nil {
		t.Error("外れないのに成功した")
	}
}

func TestUninstall(t *testing.T) {
	p := defaultPaths(t.TempDir())
	_ = os.MkdirAll(filepath.Dir(p.plistFile), 0o755)
	_ = os.WriteFile(p.plistFile, []byte("x"), 0o644)
	f := &fakeLaunchctl{loaded: true}
	if err := uninstallAgent(f.run, p, 501, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.plistFile); !os.IsNotExist(err) {
		t.Error("plist が残っている")
	}
	// Not registered (and no plist) is fine
	if err := uninstallAgent((&fakeLaunchctl{}).run, p, 501, &bytes.Buffer{}); err != nil {
		t.Errorf("未登録の uninstall が失敗した: %v", err)
	}
}

func TestUninstallKeepsPlistWhenBootoutFails(t *testing.T) {
	p := defaultPaths(t.TempDir())
	_ = os.MkdirAll(filepath.Dir(p.plistFile), 0o755)
	_ = os.WriteFile(p.plistFile, []byte("x"), 0o644)
	f := &fakeLaunchctl{bootoutErr: errors.New("exit 5: Input/output error"), bootoutOut: "Boot-out failed: 5"}
	if err := uninstallAgent(f.run, p, 501, &bytes.Buffer{}); err == nil {
		t.Error("bootout の失敗が error にならなかった")
	}
	if _, err := os.Stat(p.plistFile); err != nil {
		t.Error("読み込まれたままなのに plist を消した")
	}
}

func TestExecRunnerErrorCarriesStderr(t *testing.T) {
	_, err := execRunner("sh", "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr が error に含まれない: %v", err)
	}
}

func TestPrintStatus(t *testing.T) {
	p := defaultPaths(t.TempDir())
	_ = saveState(p.stateFile, State{Attempts: map[string]int{"U-A": 3, "U-B": 1}, UnknownStreak: map[string]int{"U-C": 3, "U-D": 1}, LastCompletedRun: "2026-11-07T17:00:00Z"})
	run := func(name string, args ...string) (string, error) {
		return "gui/501/com.wada811.cmux-agent-restore = {\n\tstate = not running\n\truns = 4\n\tlast exit code = 0\n\trun interval = 300 seconds\n}", nil
	}
	var out bytes.Buffer
	printStatus(run, p, 501, 3, &out)
	for _, want := range []string{"state=not running", "runs=4", "Last run that finished: 2026-11-07T17:00:00Z", "U-A 3/3 (gave up", "U-B 1/3\n", "U-C 3 (shown in the sidebar", "U-D 1\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status に %q が無い:\n%s", want, out.String())
		}
	}
	out.Reset()
	printStatus(func(string, ...string) (string, error) { return "", errors.New("x") }, defaultPaths(t.TempDir()), 501, 3, &out)
	if !strings.Contains(out.String(), "launchd: not registered") || !strings.Contains(out.String(), "state.json: none") {
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
	d.run = (&fakeLaunchctl{}).run
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

func TestExecRunnerEmptiesNodeOptions(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--require /tmp/evil.js")
	out, err := execRunner("sh", "-c", `printf %s "$NODE_OPTIONS"`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("子プロセスに NODE_OPTIONS が渡った: %q", out)
	}
}

func TestRestoreKindsLimitsConcurrency(t *testing.T) {
	var tabs []Tab
	for i := 0; i < 30; i++ {
		tabs = append(tabs, Tab{UUID: fmt.Sprintf("U%d", i), Ref: fmt.Sprintf("surface:%d", i)})
	}
	var cur, peak atomic.Int32
	var mu sync.Mutex
	run := func(name string, args ...string) (string, error) {
		n := cur.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
		return `{"restore_record":null}`, nil
	}
	restoreKinds(run, "cmux", tabs)
	if peak.Load() > 4 {
		t.Errorf("同時に %d 本走った, want 4 以下", peak.Load())
	}
	if peak.Load() < 2 {
		t.Errorf("並列になっていない（最大 %d）", peak.Load())
	}
}
