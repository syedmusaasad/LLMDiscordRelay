# Phase 9 (milestone 6) re-run

The m6 work (restart survival, TTL sweep, telemetry, installer, unit file, tmux/devpass
binary resolution, systemd-run scope isolation) was already landed and reviewed through
remediation phases 20-22. This re-run verified it: `make test` passes, and the
TestRestartSurvival_DisposableUnit / TestTTL_NeverSweepsLiveWindow greps, `bash -n`
checks and `%h` unit check all pass. No code changed. The commit carries the required
m6 subject so HEAD satisfies the subject predicate.
Phase 2: tracked tree not fully clean before push: install.sh (mode 644->755 only) and plans/001-build/plan.md (engine-managed decisions log) modified but uncommitted; outside brief scope, left unstaged and not pushed.

## Phase 2: integration suite on fake-claude

- Seeding is now transcript files (`txWriter` in test/integration/smoke_test.go) plus pending markers; historical
  sessions use `resume:true` markers (dead pane allowed), live panes use `resume:false`.
- fake-claude knob added: `FAKE_CLAUDE_DROP_FIRST_PROMPT=1` (first submitted prompt is lost) so the echo-verify retry
  path can be exercised. The fake only writes a transcript when a prompt is submitted, so approval/question tests
  submit a prompt in the pane before starting the relay.
- Resume refusal: `FAKE_CLAUDE_REFUSE_FIRST_RESUME` refuses every resume, so the old "refuse once, retry succeeds" case
  uses an llmux wrapper that sets it only on the first call; the double-refusal case proves the fresh-session fallback.
- Product fixes the tests exposed: echo suppression (watcher.go) swallowed the only "You:" echo after a retried
  injection; install.sh and the systemd unit still installed dpmux / devpass names and set RELAY_DPMUX_BIN.
- go.mod/go.sum: modernc.org/sqlite dropped (`go mod tidy`), nothing imports it any more.

## Phase 3: live smoke against real Claude Code (2.1.322)

- `bash test/live/claude_smoke.sh` (33 checks, ~3 min, not CI): isolated tmux socket via TMUX_TMPDIR, temp state dir,
  throwaway project under /tmp, trust dialog accepted through the relay's approval message, haiku/low/default-permissions.
  Helpers: test/live/mockctl.py (mock-discord control), test/live/refusal (runs inject.IsResumeRefused on a pane).
- Product bugs the live run exposed and this commit fixes:
  1. inject.SendText (single line): Enter was sent a few ms after the literal text; real Claude Code left the prompt typed
     but unsubmitted on fresh/resumed panes (spawn then timed out after 60 s). A 150 ms settle now separates them.
  2. relay loadEnv: with LLM_RELAY_ENV set it still migrated ~/.config/devpass-discord-relay/.env into the home config
     dir. A readable LLM_RELAY_ENV now wins and nothing under $HOME is read or migrated.
- mock-discord gained GET /control/calls (method, path, channel_id, assigned id, body) so a test can tell which thread got
  which message; existing /control/sent is unchanged.
- A dead pane whose only process is the exiting `claude` shows no text (tmux drops the output), so the refusal check
  launches claude through a shim that prints its exit status. The relay itself still detects a refusal by the pane
  signature or by the pane disappearing before ready.
- Side effects left on the host: Claude Code records trust for each throwaway /tmp/llmlive-* dir in ~/.claude.json, and
  leftover debug dirs from failed runs may remain (/tmp/llmlive-*, ~/.claude/projects/*llmlive*, -tmp-manualproj,
  -tmp-probeproj-*); a passing script run removes its own.
- INCIDENT (mine): while debugging by hand I ran `tmux kill-server` in a shell where $TMUX pointed at the host's default
  tmux server (TMUX overrides TMUX_TMPDIR), so I killed the host's default tmux server and its sessions at ~14:05. A new
  default server was running afterwards. The script now unsets TMUX and kills only its own socket explicitly (-S).
