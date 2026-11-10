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
