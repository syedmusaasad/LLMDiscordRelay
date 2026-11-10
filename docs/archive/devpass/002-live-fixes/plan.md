# Plan 002: live-CLI fixes — spawn deadlock, wrong teardown, probe latency
WORKDIR: /root/devpass-discord-relay

DECISION live-spawn-deadlock: first real-CLI run (2026-10-06) proved the Discord-spawn
path deadlocks: handleSpawnFromDiscord waits for the session identity before injecting
the first prompt, but devpass-code 1.18.11 only creates the session (and logs
message=created) after the first prompt arrives (docs/BINDING-PROTOCOL.md). The fake CLI
writes the session row at launch, hiding this in every test. Fix: inject after the
readiness probe, THEN bind from the spawn stderr log. Also: the spawn-failure teardown
killed the live session dp-1791258254443 while leaving failed spawn dp-1791258563432
running (journal 03:50:25-03:50:31); and thread replies after the first turn wait the
full 30s readiness probe because "Ask anything..." is absent post-turn (user message
"ty" took 32s to land, then worked).

## Phase 1: deliver prompt before bind, teardown the right pane, fast probe (implementer)
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: d=$(mktemp -d); make test > $d/t.log 2>&1 || { tail -60 $d/t.log; exit 1; }
EXIT: grep -rq 'func TestSpawn_PromptBeforeBind' test/integration
EXIT: d=$(mktemp -d); make integration T=TestSpawn_PromptBeforeBind > $d/s.log 2>&1; grep -q -- '--- PASS: TestSpawn_PromptBeforeBind ' $d/s.log && ! grep -q 'no tests to run' $d/s.log || { tail -60 $d/s.log; exit 1; }
EXIT: grep -rq 'func TestSpawn_FailureTeardownSparesLiveSession' test/integration
EXIT: d=$(mktemp -d); make integration T=TestSpawn_FailureTeardownSparesLiveSession > $d/f.log 2>&1; grep -q -- '--- PASS: TestSpawn_FailureTeardownSparesLiveSession ' $d/f.log && ! grep -q 'no tests to run' $d/f.log || { tail -60 $d/f.log; exit 1; }
EXIT: grep -rq 'FAKE_DP_SESSION_ON_PROMPT' cmd/fake-devpass-code/main.go
EXIT: test -s docs/REMEDIATION-24.md && ! grep -q 'PENDING' docs/REMEDIATION-24.md
EXIT: git diff --quiet HEAD -- cmd internal test docs
EXIT: git log -1 --format=%s | grep -qxF 'fix: inject first prompt before identity bind, spare live panes, fast probe'

Evidence to read first: journal lines 03:44-03:51 of 2026-10-06 in
`journalctl --user -u devpass-discord-relay`, docs/BINDING-PROTOCOL.md, and
/root/.devpass-discord-relay/stderr-dp-1791258563432-3351683.log (spawn booted, zero
message=created lines, pane never received input). Fix exactly these three defects;
never loosen an assertion or raise a timeout to hide a race.

1. Spawn order (the deadlock): in handleSpawnFromDiscord, after dpmux starts, run the
   readiness probe (capture-pane polling for the TUI ready signature, same recipe,
   pinned to 1.18.11), then send the triggering message via send-keys, and only then
   wait for identity: parse `message=created id=<ses_...>` from the spawn's stderr log
   (the marker's stderrLog path). Bind timeout starts after the send. Ambiguous or
   multiple created ids still fail closed. If the probe itself fails, teardown the
   spawned session and report in the thread — never bind, never mirror.
2. Teardown target: spawn failure must tear down ONLY the spawned tmux session. Add
   TestSpawn_FailureTeardownSparesLiveSession: a live, bound session plus a spawn that
   fails identity (fake refuses/delays created); assert the live session's pane and
   sessions.json record survive, the failed spawn's pane is gone, and the thread posts
   one failure notice.
3. Probe latency: post-turn panes lack "Ask anything..." (observed 03:44:57 capture);
   also accept the persistent input-row marker (the "Pl-Interactive" line with the
   input bar) so the common case resolves in one poll. Keep the 30s cap and 2s fallback.
   Record the observed signature from a live post-turn capture in docs/REMEDIATION-24.md.
4. Fake CLI: add FAKE_DP_SESSION_ON_PROMPT=1 — the session row and the message=created
   stderr line are written only when the first prompt arrives (real 1.18.11 behavior,
   per BINDING-PROTOCOL.md). TestSpawn_PromptBeforeBind runs the whole flow with this
   knob: channel message → spawn → prompt delivered (visible in the pane and DB) →
   bound from the created line → thread created → reply mirrored. This test must FAIL
   against the current deadlock (prove that by stashing your fix if needed) and pass
   after.
5. Update the spawn-related integration tests to the new knob where they relied on
   launch-time rows; keep every existing named invariant test passing.
6. docs/REMEDIATION-24.md maps each defect to fix and test evidence with verbatim
   command outputs; logs under docs/flake-logs/.
Stage only cmd/, internal/, test/, docs/. Never stage plans/ or .tmp/. New commit, exact
subject: `fix: inject first prompt before identity bind, spare live panes, fast probe`.

## Phase 2: publish the fix (implementer)
AFTER: 1
TIMEOUT: 900
EXIT: git fetch -q origin && git diff --quiet HEAD origin/main
EXIT: git ls-remote --tags origin | grep -q 'refs/tags/latest$'
EXIT: GIT_DIR=/root/devpass-discord-relay/.git git rev-parse latest | grep -q "$(git rev-parse HEAD)"

After phase 1's commit lands and `make test` is green: verify the working tree is clean
for tracked files, push with GIT_SSH_COMMAND='ssh -o BatchMode=yes' git push origin main,
move the release tag: git tag -f latest HEAD && GIT_SSH_COMMAND='ssh -o BatchMode=yes'
git push -f origin latest (dpmux update follows the moved tag by design). Never print
credentials. Final lines: `git ls-remote --tags origin`.
