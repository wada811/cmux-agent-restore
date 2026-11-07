package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// paths are where the state, log and plist live.
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

// State is the contents of state.json.
type State struct {
	// Attempts maps surface UUID to how many times the restore command was sent.
	// An entry is removed when the agent comes back, the tab goes away, or the record is gone.
	Attempts map[string]int `json:"attempts"`
	// UnknownStreak is surface UUID -> how many runs in a row we could not confirm that the tab is
	// safe to type into (unknown foreground, stale or shared tty). Dropped as soon as it is confirmed.
	UnknownStreak map[string]int `json:"unknown_streak,omitempty"`
	// LastCompletedRun is when the last non-dry-run run finished all the way through (RFC 3339).
	// A run that stopped early does not update it.
	LastCompletedRun string `json:"last_completed_run,omitempty"`
}

// loadState reads state.json. If it is missing or broken, it returns an empty state.
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

// saveState writes state.json. It writes a temporary file and renames it, so being stopped midway cannot corrupt it.
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

var errLocked = errors.New("another process is running")

// acquireLock takes an exclusive lock, or returns errLocked. Call the returned function to release it.
// The OS drops the lock when the process dies, so a leftover lock file does not block anything.
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
