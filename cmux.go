package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// bundledCmux は PATH に cmux が無いときに使う、cmux.app 同梱の CLI。
const bundledCmux = "/Applications/cmux.app/Contents/Resources/bin/cmux"

// commandTimeout は外部コマンド 1 回の上限。
const commandTimeout = 15 * time.Second

// findCmux は cmux の場所を返す。PATH から探し、無ければ cmux.app 同梱の CLI。
func findCmux(lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) (string, error) {
	if p, err := lookPath("cmux"); err == nil {
		return p, nil
	}
	if _, err := stat(bundledCmux); err == nil {
		return bundledCmux, nil
	}
	return "", errNoCmux
}

// execRunner は実際にコマンドを実行する Runner。失敗しても取れた出力は返す。
func execRunner(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// 起動元が持つ Node のフラグ（--require など）を cmux の CLI に引き継がない
	cmd.Env = append(os.Environ(), "NODE_OPTIONS=")
	out, err := cmd.Output()
	return string(out), err
}

// Tab は cmux tree の terminal タブ。
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

// parseTree は `cmux tree --all --json` の出力から、全 workspace の ref と terminal タブの一覧を作る。
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

// parseRestoreKind は `cmux surface resume show --json` の出力から、復元記録（checkpoint_id 付き）の kind を返す。
// 記録が無い・読めなければ空文字。kind が無ければ "unknown"。
func parseRestoreKind(out string) string {
	var j struct {
		RestoreRecord *struct {
			CheckpointID any `json:"checkpoint_id"`
			Kind         any `json:"kind"`
		} `json:"restore_record"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil || j.RestoreRecord == nil {
		return ""
	}
	if truthyEmpty(j.RestoreRecord.CheckpointID) {
		return ""
	}
	if truthyEmpty(j.RestoreRecord.Kind) {
		return "unknown"
	}
	return fmt.Sprint(j.RestoreRecord.Kind)
}

// truthyEmpty は JSON の値が null・空文字・false・0 のとき true（JavaScript の falsy に合わせる）。
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

// restoreKinds は各タブの復元記録の kind を並列に読む。記録が無い・読めないタブは空文字。
func restoreKinds(run Runner, cmux string, tabs []Tab) []string {
	kinds := make([]string, len(tabs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, t := range tabs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out, err := run(cmux, "surface", "resume", "show", "--json", "--surface", t.UUID)
			if err != nil {
				return
			}
			kinds[i] = parseRestoreKind(out)
		}()
	}
	wg.Wait()
	return kinds
}
