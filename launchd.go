package main

import (
	"bytes"
	"encoding/xml"
	"errors"
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

const (
	// bootoutWaitTries · bootoutWaitInterval: launchctl bootout returns before the service is gone,
	// and a bootstrap right after it fails with "Bootstrap failed: 5: Input/output error".
	bootoutWaitTries    = 25
	bootoutWaitInterval = 200 * time.Millisecond
	bootstrapTries      = 5
	bootstrapInterval   = time.Second
)

// writeFileAtomic writes data to a temporary file next to path and renames it over path, so a
// reader never sees a half-written plist. If path is a symlink, the symlink itself is replaced
// (launchd does not load a symlinked plist automatically).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// isNotRegistered reports whether a failed launchctl bootout means "the service was not loaded".
func isNotRegistered(out string, err error) bool {
	text := out
	if err != nil {
		text += " " + err.Error()
	}
	return strings.Contains(text, "No such process") || strings.Contains(text, "Could not find service")
}

// installAgent writes the plist as a regular file (atomically) and loads it with launchctl bootstrap.
// If it is already loaded, it is booted out first. If loading fails, the previous plist is put back
// and loaded again, so a failed install does not leave the machine without the job.
func installAgent(run Runner, sleep func(time.Duration), p paths, exe string, runArgs []string, interval time.Duration, uid int, user string, stdout io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(p.plistFile), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.logFile), 0o755); err != nil {
		return err
	}
	target := domainTarget(uid) + "/" + launchdLabel
	oldPlist, readErr := os.ReadFile(p.plistFile)
	hadOld := readErr == nil

	rollback := func(cause error) error {
		var restoreErr error
		if hadOld {
			restoreErr = writeFileAtomic(p.plistFile, oldPlist, 0o644)
			if restoreErr == nil {
				if out, err := run("launchctl", "bootstrap", domainTarget(uid), p.plistFile); err != nil {
					restoreErr = fmt.Errorf("could not load the previous plist again: %v %s", err, strings.TrimSpace(out))
				}
			}
		} else {
			os.Remove(p.plistFile)
		}
		if restoreErr != nil {
			return fmt.Errorf("%w (and the rollback failed: %v)", cause, restoreErr)
		}
		return fmt.Errorf("%w (the previous plist was put back)", cause)
	}

	if err := writeFileAtomic(p.plistFile, []byte(plistXML(exe, runArgs, interval, p.home, user)), 0o644); err != nil {
		return err
	}
	if out, err := run("launchctl", "bootout", target); err != nil && !isNotRegistered(out, err) {
		return rollback(fmt.Errorf("launchctl bootout failed: %v %s", err, strings.TrimSpace(out)))
	}
	// Wait until launchd has really dropped the job
	gone := false
	for i := 0; i < bootoutWaitTries; i++ {
		if _, err := run("launchctl", "print", target); err != nil {
			gone = true
			break
		}
		sleep(bootoutWaitInterval)
	}
	if !gone {
		return rollback(errors.New("the job is still registered after launchctl bootout"))
	}
	var lastErr error
	for i := 0; i < bootstrapTries; i++ {
		if i > 0 {
			sleep(bootstrapInterval)
		}
		out, err := run("launchctl", "bootstrap", domainTarget(uid), p.plistFile)
		if err == nil {
			fmt.Fprintf(stdout, "Installed %s (runs every %s)\n", p.plistFile, interval)
			return nil
		}
		lastErr = fmt.Errorf("launchctl bootstrap failed: %v %s", err, strings.TrimSpace(out))
	}
	return rollback(lastErr)
}

// uninstallAgent boots the job out of launchd and removes the plist. If bootout fails for any reason
// other than "not registered", the plist is kept: removing it would leave a loaded job with no file.
func uninstallAgent(run Runner, p paths, uid int, stdout io.Writer) error {
	if out, err := run("launchctl", "bootout", domainTarget(uid)+"/"+launchdLabel); err != nil {
		if !isNotRegistered(out, err) {
			return fmt.Errorf("launchctl bootout failed, plist kept: %v %s", err, strings.TrimSpace(out))
		}
		fmt.Fprintln(stdout, "The job was not registered with launchd")
	}
	if err := os.Remove(p.plistFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(stdout, "Uninstalled %s\n", p.plistFile)
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
