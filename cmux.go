package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// bundledCmux is the CLI inside cmux.app, used when cmux is not on PATH.
const bundledCmux = "/Applications/cmux.app/Contents/Resources/bin/cmux"

// commandTimeout is the limit for one external command.
const commandTimeout = 15 * time.Second

// findCmux returns where cmux is: on PATH, or else the CLI inside cmux.app.
func findCmux(lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) (string, error) {
	if p, err := lookPath("cmux"); err == nil {
		return p, nil
	}
	if _, err := stat(bundledCmux); err == nil {
		return bundledCmux, nil
	}
	return "", errNoCmux
}

// execRunner is the Runner that really runs commands. It returns what it got on stdout even on failure.
func execRunner(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Do not pass the caller's Node flags (--require and so on) on to the cmux CLI
	cmd.Env = append(os.Environ(), "NODE_OPTIONS=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		err = &cmdError{err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return string(out), err
}

// cmdError is a failed command with what it wrote to stderr, so callers and logs can say why.
type cmdError struct {
	err    error
	stderr string
}

func (e *cmdError) Error() string {
	if e.stderr == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + e.stderr
}

func (e *cmdError) Unwrap() error { return e.err }

// Tab is a terminal tab in `cmux tree`.
type Tab struct {
	UUID         string
	Ref          string
	WorkspaceRef string
	TTY          string
}

type treeJSON struct {
	Windows []struct {
		Workspaces []struct {
			Ref   string `json:"ref"`
			Panes []struct {
				Surfaces []struct {
					ID   string `json:"id"`
					Ref  string `json:"ref"`
					Type string `json:"type"`
					TTY  string `json:"tty"`
				} `json:"surfaces"`
			} `json:"panes"`
		} `json:"workspaces"`
	} `json:"windows"`
}

// parseTree builds the refs of all workspaces and the list of terminal tabs from `cmux tree --all --json`.
func parseTree(out string) (workspaceRefs []string, tabs []Tab, err error) {
	var tree treeJSON
	if err := json.Unmarshal([]byte(out), &tree); err != nil {
		return nil, nil, err
	}
	for _, win := range tree.Windows {
		for _, ws := range win.Workspaces {
			workspaceRefs = append(workspaceRefs, ws.Ref)
			for _, pane := range ws.Panes {
				for _, s := range pane.Surfaces {
					if s.Type == "terminal" {
						tabs = append(tabs, Tab{UUID: s.ID, Ref: s.Ref, WorkspaceRef: ws.Ref, TTY: s.TTY})
					}
				}
			}
		}
	}
	return workspaceRefs, tabs, nil
}

// parseRestoreKind returns the kind of the restore record (one with a checkpoint_id) from the output of
// `cmux surface resume show --json`. It returns "" when there is no record and "unknown" when the record
// has no kind. The error is non-nil only when the output cannot be read: that is different from "no
// record", and the caller must not treat the tab as record-less.
func parseRestoreKind(out string) (string, error) {
	var j struct {
		RestoreRecord *struct {
			CheckpointID any `json:"checkpoint_id"`
			Kind         any `json:"kind"`
		} `json:"restore_record"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		return "", err
	}
	if j.RestoreRecord == nil || truthyEmpty(j.RestoreRecord.CheckpointID) {
		return "", nil
	}
	if truthyEmpty(j.RestoreRecord.Kind) {
		return "unknown", nil
	}
	return fmt.Sprint(j.RestoreRecord.Kind), nil
}

// truthyEmpty is true when a JSON value is null, an empty string, false or 0 (like JavaScript falsy).
func truthyEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	}
	return false
}

// recordReadConcurrency is how many `cmux surface resume show` run at once. cmux answers each one
// from the app, so a high number makes them time out.
const recordReadConcurrency = 4

// restoreKinds reads the restore record kind of each tab in parallel. kinds[i] is "" when tab i has no
// record. readErrs[i] is non-nil when the record could not be read (the command failed or its output
// was not JSON); kinds[i] is "" then too, but that does not mean there is no record.
func restoreKinds(run Runner, cmux string, tabs []Tab) (kinds []string, readErrs []error) {
	kinds = make([]string, len(tabs))
	readErrs = make([]error, len(tabs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, recordReadConcurrency)
	for i, t := range tabs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out, err := run(cmux, "surface", "resume", "show", "--json", "--surface", t.UUID)
			if err != nil {
				readErrs[i] = err
				return
			}
			kinds[i], readErrs[i] = parseRestoreKind(out)
		}()
	}
	wg.Wait()
	return kinds, readErrs
}
