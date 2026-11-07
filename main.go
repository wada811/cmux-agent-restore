// Command cmux-agent-restore re-sends cmux restore to Claude Code and Codex tabs that cmux failed to restore.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// version is set by GoReleaser through -ldflags.
var version = "dev"

// versionString uses the module version for `go install` builds, which carry no ldflags.
func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return version
}

// deps is how main reaches the outside. Tests replace it.
type deps struct {
	run      Runner
	paths    paths
	lookPath func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
	sleep    func(time.Duration)
	now      func() time.Time
	exe      func() (string, error)
	uid      int
	user     string
}

func realDeps() deps {
	home, _ := os.UserHomeDir()
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return deps{
		run:      execRunner,
		paths:    defaultPaths(home),
		lookPath: exec.LookPath,
		stat:     os.Stat,
		sleep:    time.Sleep,
		now:      time.Now,
		exe:      os.Executable,
		uid:      os.Getuid(),
		user:     name,
	}
}

// runFlags are the flag values shared by run and install.
type runFlags struct {
	dryRun        bool
	batch         int
	batchInterval time.Duration
	maxAttempts   int
	kinds         string
	interval      time.Duration
}

func newFlagSet(name string, withDryRun, withInterval bool) (*flag.FlagSet, *runFlags) {
	f := &runFlags{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if withDryRun {
		fs.BoolVar(&f.dryRun, "dry-run", false, "do not send or update state")
	}
	if withInterval {
		fs.DurationVar(&f.interval, "interval", 5*time.Minute, "interval between runs")
	}
	fs.IntVar(&f.batch, "batch", defaultBatch, "tabs to send to per batch")
	fs.DurationVar(&f.batchInterval, "batch-interval", defaultBatchInterval, "pause between batches")
	fs.IntVar(&f.maxAttempts, "max-attempts", defaultMaxAttempts, "sends before giving up on a tab")
	fs.StringVar(&f.kinds, "kinds", strings.Join(DefaultKinds, ","), "restore_record kinds to target (comma separated)")
	return fs, f
}

func parseKinds(s string) []string {
	var kinds []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// options validates the flag values and turns them into Options.
func (f *runFlags) options() (Options, error) {
	kinds := parseKinds(f.kinds)
	switch {
	case f.batch < 1:
		return Options{}, errors.New("--batch must be 1 or more")
	case f.maxAttempts < 1:
		return Options{}, errors.New("--max-attempts must be 1 or more")
	case f.batchInterval < 0:
		return Options{}, errors.New("--batch-interval must be 0 or more")
	case len(kinds) == 0:
		return Options{}, errors.New("--kinds is empty")
	}
	return Options{DryRun: f.dryRun, Batch: f.batch, BatchInterval: f.batchInterval, MaxAttempts: f.maxAttempts, Kinds: kinds}, nil
}

// setRunArgs lists only the flags that were given explicitly, for install to put in the run arguments of the plist.
func setRunArgs(fs *flag.FlagSet) []string {
	var args []string
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "batch", "batch-interval", "max-attempts", "kinds":
			args = append(args, "--"+fl.Name, fl.Value.String())
		}
	})
	return args
}

func usageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "cmux-agent-restore: %s\n%s\nSee 'cmux-agent-restore --help' for details.\n", msg, usageLine)
	return 2
}

func execute(args []string, stdout, stderr io.Writer, d deps) int {
	if len(args) == 0 {
		return usageError(stderr, "give a subcommand")
	}
	switch args[0] {
	case "--version", "-version", "version":
		fmt.Fprintf(stdout, "cmux-agent-restore %s\n", versionString())
		return 0
	case "-h", "--help", "-help", "help":
		io.WriteString(stdout, helpText)
		return 0
	case "run":
		return cmdRun(args[1:], stdout, stderr, d)
	case "install":
		return cmdInstall(args[1:], stdout, stderr, d)
	case "uninstall":
		if len(args) > 1 {
			return usageError(stderr, "uninstall takes no arguments: "+strings.Join(args[1:], " "))
		}
		if err := uninstallAgent(d.run, d.paths, d.uid, stdout); err != nil {
			fmt.Fprintln(stderr, "cmux-agent-restore:", err)
			return 1
		}
		return 0
	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		max := fs.Int("max-attempts", defaultMaxAttempts, "")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				io.WriteString(stdout, helpText)
				return 0
			}
			return usageError(stderr, err.Error())
		}
		printStatus(d.run, d.paths, d.uid, *max, stdout)
		return 0
	}
	return usageError(stderr, "unknown subcommand: "+args[0])
}

// parseRunFlags reads the flags of run and install. If the exit code is -1, carry on.
func parseRunFlags(name string, args []string, withDryRun, withInterval bool, stdout, stderr io.Writer) (*flag.FlagSet, *runFlags, Options, int) {
	fs, f := newFlagSet(name, withDryRun, withInterval)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			io.WriteString(stdout, helpText)
			return nil, nil, Options{}, 0
		}
		return nil, nil, Options{}, usageError(stderr, err.Error())
	}
	if fs.NArg() > 0 {
		return nil, nil, Options{}, usageError(stderr, "unexpected argument: "+strings.Join(fs.Args(), " "))
	}
	opt, err := f.options()
	if err != nil {
		return nil, nil, Options{}, usageError(stderr, err.Error())
	}
	return fs, f, opt, -1
}

// lineLogger writes timestamped lines to stdout and the log file. It does not write the file in dry-run.
type lineLogger struct {
	out    io.Writer
	file   *os.File
	dryRun bool
	now    func() time.Time
}

func (l *lineLogger) Logf(format string, args ...any) {
	prefix := ""
	if l.dryRun {
		prefix = "[dry-run] "
	}
	line := fmt.Sprintf("%s %s%s", l.now().Format("2006-01-02 15:04:05"), prefix, fmt.Sprintf(format, args...))
	fmt.Fprintln(l.out, line)
	if l.file != nil {
		fmt.Fprintln(l.file, line)
	}
}

func cmdRun(args []string, stdout, stderr io.Writer, d deps) int {
	_, _, opt, code := parseRunFlags("run", args, true, false, stdout, stderr)
	if code >= 0 {
		return code
	}
	cmux, err := findCmux(d.lookPath, d.stat)
	if err != nil {
		fmt.Fprintf(stderr, "cmux-agent-restore: %v (looked on PATH and at %s)\n", err, bundledCmux)
		return 1
	}
	log := &lineLogger{out: stdout, dryRun: opt.DryRun, now: d.now}
	if !opt.DryRun {
		release, err := acquireLock(d.paths.lockFile)
		if errors.Is(err, errLocked) {
			fmt.Fprintln(stdout, "another process is running, doing nothing")
			return 0
		}
		if err != nil {
			fmt.Fprintf(stderr, "cmux-agent-restore: could not take the lock: %v\n", err)
			return 1
		}
		defer release()
		if err := os.MkdirAll(filepath.Dir(d.paths.logFile), 0o755); err == nil {
			if f, err := os.OpenFile(d.paths.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				defer f.Close()
				log.file = f
			}
		}
	}
	env := &Env{
		Run: d.run, Cmux: cmux, Sleep: d.sleep, Logf: log.Logf,
		StatePath: d.paths.stateFile, Now: d.now, PresenceInterval: presenceInterval,
	}
	if err := Restore(env, opt); err != nil {
		log.Logf("ERROR %v", err)
		return 1
	}
	return 0
}

func cmdInstall(args []string, stdout, stderr io.Writer, d deps) int {
	fs, f, _, code := parseRunFlags("install", args, false, true, stdout, stderr)
	if code >= 0 {
		return code
	}
	if f.interval < time.Minute {
		return usageError(stderr, "--interval must be 1m or more")
	}
	exe, err := d.exe()
	if err != nil {
		fmt.Fprintf(stderr, "cmux-agent-restore: cannot find the executable: %v\n", err)
		return 1
	}
	exe = stableExePath(exe, d.lookPath)
	if err := installAgent(d.run, d.sleep, d.paths, exe, setRunArgs(fs), f.interval, d.uid, d.user, stdout); err != nil {
		fmt.Fprintln(stderr, "cmux-agent-restore:", err)
		return 1
	}
	return 0
}

// stableExePath returns the location on PATH if cmux-agent-restore on PATH is the same file as the one running.
// Homebrew puts a symlink in /opt/homebrew/bin, so writing the versioned real path into the plist
// would break on upgrade.
func stableExePath(exe string, lookPath func(string) (string, error)) string {
	p, err := lookPath(appName)
	if err != nil {
		return exe
	}
	a, errA := os.Stat(p)
	b, errB := os.Stat(exe)
	if errA == nil && errB == nil && os.SameFile(a, b) {
		return p
	}
	return exe
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr, realDeps()))
}
