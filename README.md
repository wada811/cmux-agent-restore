# cmux-agent-restore

Re-send `cmux restore` to the Claude Code and Codex tabs that [cmux](https://github.com/manaflow-ai/cmux) failed to bring back.

After you restart cmux, some tabs come back as a bare shell even though cmux still holds a restore record for them. In one restart with cmux 0.65.2, 50 of 208 tabs were left like this. This tool finds those tabs and types `cmux restore --surface <surface-uuid>` into their shell, a few at a time, until the agent is back.

macOS only (cmux is macOS only). A single Go binary with no dependencies.

## Install

```sh
brew install wada811/tap/cmux-agent-restore   # once the tap is published
cmux-agent-restore install                    # run every 5 minutes via launchd
```

Or build it yourself:

```sh
go install github.com/wada811/cmux-agent-restore@latest
```

## Commands

| Command | What it does |
| --- | --- |
| `run` | Look over the tabs once and send `cmux restore` to the ones that were not restored. `--dry-run` sends nothing and writes no state. |
| `install` | Write a launchd plist to `~/Library/LaunchAgents` (a real file, not a symlink) and `launchctl bootstrap` it. `--interval` sets the period (default 5m). |
| `uninstall` | `launchctl bootout` the job and remove the plist. |
| `status` | Show the launchd state, the last run and the contents of `state.json`. |

Flags for `run` (`install` bakes the ones you pass into the plist):

| Flag | Default | Meaning |
| --- | --- | --- |
| `--batch N` | 3 | Tabs per batch. |
| `--batch-interval DUR` | 20s | Pause between batches (cmux hit `pool_saturated` when everything was restored at once). |
| `--max-attempts N` | 3 | Stop sending to a tab after this many tries. |
| `--kinds LIST` | `claude,codex` | `restore_record` kinds to restore. |

There is no config file. Pass flags instead.

## When a tab gets the command

A tab receives `cmux restore` only if all of these hold:

1. Its restore record has a `checkpoint_id` and its `kind` is one of `--kinds`.
2. No agent runs in the tab. Agents are found with `ps` by executable name (`claude`, `codex`) and matched to tabs through the `CMUX_SURFACE_ID` variable of each process. The check is repeated right before sending, 1.5 seconds apart, and both checks must find nothing.
3. The foreground processes on the tab's tty are only shells (zsh, bash, sh, dash, fish), and that tty does not belong to another tab. This keeps the command out of editors, and out of a running Codex that would take it as a prompt.

A tab that fails check 2 or 3 right before sending is skipped and the attempt is not counted. After `--max-attempts` sends that did not bring the agent back, the tool stops sending to the tab and shows `Needs restore N` in the sidebar status of its workspace (`agent_restore`); restore it by hand. The status is re-evaluated for every workspace on each run.

## Files

| Path | Content |
| --- | --- |
| `~/Library/Application Support/cmux-agent-restore/state.json` | Number of sends per tab, and the time of the last run. |
| `~/Library/Application Support/cmux-agent-restore/run.lock` | Lock so two runs never overlap. A second run exits quietly. |
| `~/Library/Logs/cmux-agent-restore.log` | Log of non-dry runs. |
| `~/Library/Logs/cmux-agent-restore.err.log` | Standard error of the launchd job. |

`cmux` is found on `PATH`, then at `/Applications/cmux.app/Contents/Resources/bin/cmux`. If cmux does not answer, the run does nothing.

## Codex tabs need a restore record

This tool cannot restore a tab that has no record. Codex leaves a record only when it is started through cmux's relay (the per-tab shim that cmux puts under `~/.cmuxterm/cmux-cli-shims/`) and with `--no-daemon`. A shell function like this does it:

```zsh
codex() {
  local shim="$HOME/.cmuxterm/cmux-cli-shims/${CMUX_SURFACE_ID:-}/codex"
  if [[ -z "${CMUX_SURFACE_ID:-}" || ! -x "$shim" ]]; then
    command codex "$@"
    return
  fi
  if [[ $# -eq 0 || "$1" == -* || "$1" == resume || "$1" == fork ]]; then
    "$shim" --no-daemon "$@"
  else
    "$shim" "$@"
  fi
}
```

## Development

```sh
go vet ./...
go test ./...
goreleaser check
goreleaser build --snapshot --clean
```

Calls to `cmux`, `ps` and `launchctl` go through a `Runner` function, so the decision logic is tested without cmux.

## License

MIT
