package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// plistXML は launchd の plist を作る。
// ログは本体が ~/Library/Logs/cmux-agent-restore.log に書くので、標準出力は捨てる
// （~/Documents 配下は launchd から書けないため、置き場所は ~/Library/Logs）。
func plistXML(exe string, runArgs []string, interval time.Duration, home, user string) string {
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var args strings.Builder
	for _, a := range append([]string{exe, "run"}, runArgs...) {
		fmt.Fprintf(&args, "        <string>%s</string>\n", esc(a))
	}
	errLog := filepath.Join(home, "Library", "Logs", appName+".err.log")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>

    <key>ProgramArguments</key>
    <array>
%s    </array>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
        <key>HOME</key>
        <string>%s</string>
        <key>USER</key>
        <string>%s</string>
        <key>LOGNAME</key>
        <string>%s</string>
    </dict>

    <key>StartInterval</key>
    <integer>%d</integer>

    <key>RunAtLoad</key>
    <false/>

    <key>ProcessType</key>
    <string>Background</string>

    <key>StandardOutPath</key>
    <string>/dev/null</string>

    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, launchdLabel, args.String(), esc(home), esc(user), esc(user), int(interval.Seconds()), esc(errLog))
}

func domainTarget(uid int) string { return fmt.Sprintf("gui/%d", uid) }

// installAgent は plist を実ファイル（symlink でなく）で書き、launchctl bootstrap で読み込む。
// すでに読み込まれていれば、いったん外してから読み込み直す。
func installAgent(run Runner, p paths, exe string, runArgs []string, interval time.Duration, uid int, user string, stdout io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(p.plistFile), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.logFile), 0o755); err != nil {
		return err
	}
	// symlink だと launchd が自動で読み込まないので、あれば消して実ファイルで置く
	os.Remove(p.plistFile)
	if err := os.WriteFile(p.plistFile, []byte(plistXML(exe, runArgs, interval, p.home, user)), 0o644); err != nil {
		return err
	}
	run("launchctl", "bootout", domainTarget(uid)+"/"+launchdLabel) // 未登録なら失敗するので無視する
	if out, err := run("launchctl", "bootstrap", domainTarget(uid), p.plistFile); err != nil {
		return fmt.Errorf("launchctl bootstrap に失敗: %v %s", err, strings.TrimSpace(out))
	}
	fmt.Fprintf(stdout, "インストールした: %s（%s ごとに実行）\n", p.plistFile, interval)
	return nil
}

// uninstallAgent は launchd から外し、plist を消す。
func uninstallAgent(run Runner, p paths, uid int, stdout io.Writer) error {
	if out, err := run("launchctl", "bootout", domainTarget(uid)+"/"+launchdLabel); err != nil {
		fmt.Fprintf(stdout, "launchd に登録されていなかった（%s）\n", strings.TrimSpace(out))
	}
	if err := os.Remove(p.plistFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(stdout, "アンインストールした: %s\n", p.plistFile)
	return nil
}

var launchctlFieldRe = regexp.MustCompile(`(?m)^\s*(state|runs|last exit code|run interval) = (.+)$`)

// parseLaunchctlPrint は `launchctl print` の出力から state・runs・last exit code・run interval を拾う。
func parseLaunchctlPrint(out string) map[string]string {
	fields := map[string]string{}
	for _, m := range launchctlFieldRe.FindAllStringSubmatch(out, -1) {
		if _, seen := fields[m[1]]; !seen {
			fields[m[1]] = strings.TrimSpace(m[2])
		}
	}
	return fields
}

// printStatus は launchd の状態・最後の実行・state.json の中身を出す。
func printStatus(run Runner, p paths, uid int, maxAttempts int, stdout io.Writer) {
	if _, err := os.Stat(p.plistFile); err != nil {
		fmt.Fprintf(stdout, "plist: なし（%s）\n", p.plistFile)
	} else {
		fmt.Fprintf(stdout, "plist: %s\n", p.plistFile)
	}
	if out, err := run("launchctl", "print", domainTarget(uid)+"/"+launchdLabel); err != nil {
		fmt.Fprintln(stdout, "launchd: 未登録")
	} else {
		f := parseLaunchctlPrint(out)
		fmt.Fprintf(stdout, "launchd: 登録済み（state=%s・実行回数=%s・直近の終了コード=%s・間隔=%s）\n",
			orDash(f["state"]), orDash(f["runs"]), orDash(f["last exit code"]), orDash(f["run interval"]))
	}

	state := loadState(p.stateFile)
	if _, err := os.Stat(p.stateFile); err != nil {
		fmt.Fprintf(stdout, "state.json: なし（%s）\n", p.stateFile)
		return
	}
	last := "なし"
	if state.LastRun != "" {
		last = state.LastRun
	}
	fmt.Fprintf(stdout, "最後の実行: %s\n", last)
	fmt.Fprintf(stdout, "state.json: %s\n", p.stateFile)
	if len(state.Attempts) == 0 {
		fmt.Fprintln(stdout, "試行中・打ち切りのタブ: なし")
		return
	}
	uuids := make([]string, 0, len(state.Attempts))
	for u := range state.Attempts {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	fmt.Fprintln(stdout, "タブごとの送信回数:")
	for _, u := range uuids {
		n := state.Attempts[u]
		mark := ""
		if n >= maxAttempts {
			mark = "（打ち切り。手で戻すタブ）"
		}
		fmt.Fprintf(stdout, "  %s %s/%s%s\n", u, strconv.Itoa(n), strconv.Itoa(maxAttempts), mark)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
