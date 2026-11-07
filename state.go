package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// paths は状態・ログ・plist の置き場所。
type paths struct {
	home      string
	stateDir  string
	stateFile string
	lockFile  string
	logFile   string
	plistFile string
}

const (
	launchdLabel = "com.wada811.cmux-agent-restore"
	appName      = "cmux-agent-restore"
)

func defaultPaths(home string) paths {
	dir := filepath.Join(home, "Library", "Application Support", appName)
	return paths{
		home:      home,
		stateDir:  dir,
		stateFile: filepath.Join(dir, "state.json"),
		lockFile:  filepath.Join(dir, "run.lock"),
		logFile:   filepath.Join(home, "Library", "Logs", appName+".log"),
		plistFile: filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"),
	}
}

// State は state.json の中身。
type State struct {
	// Attempts は surface UUID → 復元コマンドを送った回数。
	// エージェントが戻る・タブが消える・記録が無くなると外す。
	Attempts map[string]int `json:"attempts"`
	// UnknownStreak is surface UUID -> how many runs in a row we could not confirm that the tab is
	// safe to type into (unknown foreground, stale or shared tty). Dropped as soon as it is confirmed.
	UnknownStreak map[string]int `json:"unknown_streak,omitempty"`
	// LastCompletedRun is when the last non-dry-run run finished all the way through (RFC 3339).
	// A run that stopped early does not update it.
	LastCompletedRun string `json:"last_completed_run,omitempty"`
}

// loadState は state.json を読む。無い・壊れている場合は空の状態。
func loadState(path string) State {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{Attempts: map[string]int{}, UnknownStreak: map[string]int{}}
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		s = State{}
	}
	if s.Attempts == nil {
		s.Attempts = map[string]int{}
	}
	if s.UnknownStreak == nil {
		s.UnknownStreak = map[string]int{}
	}
	return s
}

// saveState は state.json を書く。途中で止められても壊れないよう、一時ファイルに書いて置き換える。
func saveState(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var errLocked = errors.New("別のプロセスが実行中")

// acquireLock は排他ロックを取る。取れなければ errLocked。返した関数でロックを手放す。
// プロセスが死ねば OS がロックを外すので、ロックファイルが残っても詰まらない。
func acquireLock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
