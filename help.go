package main

const usageLine = "usage: cmux-agent-restore <run|install|uninstall|status> [flags] | --version | --help"

// helpText is written to be read by people and by LLM agents: what the tool does, what each value
// means, and what to do next.
const helpText = `cmux-agent-restore - re-send "cmux restore" to Claude Code and Codex tabs that cmux failed to restore.

After cmux restarts, some tabs have a restore record (restore_record) but the agent never
starts and the tab stays at a bare shell. This tool types
"cmux restore --surface <surface-uuid>" into the shell of such a tab to bring the agent back.

` + usageLine + `

Subcommands:
  run         Look over all tabs once and send the restore command to tabs that were not restored
  install     Write a launchd plist to ~/Library/LaunchAgents and bootstrap it (every 5 minutes by default)
  uninstall   Remove the job from launchd and delete the plist
  status      Show the launchd state, the last run that finished, and the contents of state.json

Flags of run:
  --dry-run              Do not send or update state; only print the targets and the checks
  --batch N              How many tabs to send to per batch (default 3)
  --batch-interval DUR   Pause between batches (default 20s), to avoid pool_saturated
  --max-attempts N       Stop sending to a tab that has not come back after this many sends, and show
                         it in the sidebar (default 3)
  --kinds LIST           restore_record kinds to target (comma separated; default claude,codex)

Flags of install:
  --interval DUR         Interval between runs (default 5m)
  --batch, --batch-interval, --max-attempts, --kinds
                         Only the ones you give are written into the run arguments of the plist

Flags of status:
  --max-attempts N       Number of attempts used to decide a tab is given up on (default 3)

When it sends (only to a tab that meets all of these):
  1. The kind of its restore record is a targeted kind
  2. No agent is running on the tab (checked twice, 1.5 seconds apart, right before sending)
  3. Only a shell (zsh, bash, sh, dash, fish) is in the foreground, and the tty is not shared with or
     taken over by another tab
A tab where 3 cannot be confirmed (an editor in front, Codex running, and so on) gets nothing and the
attempt is not counted. If that goes on for 3 runs in a row, the tab is shown in the sidebar as
"cannot verify", separately from tabs that need a manual restore.

Locations:
  state  ~/Library/Application Support/cmux-agent-restore/state.json
  log    ~/Library/Logs/cmux-agent-restore.log
  cmux   Looked up on PATH, else /Applications/cmux.app/Contents/Resources/bin/cmux

Options:
  --version              Print the version and exit
  -h, --help             Print this help
`
