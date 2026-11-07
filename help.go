package main

const usageLine = "usage: cmux-agent-restore <run|install|uninstall|status> [flags] | --version | --help"

// helpText は人にも LLM エージェントにも読める形で、何をするツールか・各値の意味・次にすることを書く。
const helpText = `cmux-agent-restore - cmux が戻し損ねた Claude Code・Codex のタブに cmux restore を送り直す。

cmux を再起動すると、復元記録（restore_record）があるのにエージェントが起動せず、
シェルのまま止まるタブが出る。このツールは、そのタブのシェルに
"cmux restore --surface <ref>" を打ち込んで戻す。

` + usageLine + `

サブコマンド:
  run         1 回見回って、戻し損ねたタブに送る
  install     launchd の plist を ~/Library/LaunchAgents に書いて bootstrap する（既定 5 分間隔）
  uninstall   launchd から外し、plist を消す
  status      launchd の状態・最後の実行・state.json の中身を出す

run のフラグ:
  --dry-run              送信も状態の更新もせず、対象と判定だけを出す
  --batch N              1 組あたりの送信数（既定 3）
  --batch-interval DUR   組と組の間隔（既定 20s）。pool_saturated を避けるため
  --max-attempts N       この回数送っても戻らないタブには送らず、サイドバーに出す（既定 3）
  --kinds LIST           対象にする restore_record の kind（カンマ区切り。既定 claude,codex）

install のフラグ:
  --interval DUR         実行の間隔（既定 5m）
  --batch・--batch-interval・--max-attempts・--kinds
                         指定したものだけ plist の run の引数に入れる

status のフラグ:
  --max-attempts N       打ち切りの判定に使う回数（既定 3）

送る条件（すべて満たすタブにだけ送る）:
  1. 復元記録の kind が対象の種類
  2. そのタブでエージェントが動いていない（送る直前に 1.5 秒あけて 2 回見て、2 回ともいない）
  3. 前面のプロセスがシェルだけ（zsh・bash・sh・dash・fish）で、tty が別のタブを指していない
  3 が確かめられないタブ（エディタが前面・Codex 実行中など）には送らず、試行回数にも数えない。

置き場所:
  状態  ~/Library/Application Support/cmux-agent-restore/state.json
  ログ  ~/Library/Logs/cmux-agent-restore.log
  cmux  PATH から探し、無ければ /Applications/cmux.app/Contents/Resources/bin/cmux

Options:
  --version              バージョンを出して終わる
  -h, --help             このヘルプを出す
`
