# Plan 001: DevPassDiscordRelay built in six milestones on a DB-tailed transcript
WORKDIR: /root/devpass-discord-relay

DECISION transcript-source: operator chose (b) after docs/DISCOVERY.md reported
PREMISE-TRANSCRIPT-ON-DISK: FAIL. The transcript stream is devpass-code's SQLite store
(~/.local/share/devpass-code/opencode.db, WAL mode) tailed READ-ONLY. "New transcript
file" in the spec means "new row in the session table created after spawn start";
"tail the transcript" means poll message/part rows for that session id. The
transcript-vs-pane split is unchanged: content from the DB, prompts/readiness/refusal
from capture-pane. Never invert it.
DECISION toolchain-install: box had no go/docker/make (DISCOVERY.md §6). Phase 1 installs
Go (official tarball to /usr/local/go), make, and docker.io so the spec's static binary
and Docker integration suite are buildable. Nothing else is installed system-wide.
DECISION sqlite-driver: modernc.org/sqlite (pure Go) so the binary stays static
(CGO_ENABLED=0). Open with URI file:<path>?mode=ro; never write to opencode.db.
DECISION discovered-facts: devpass-code 1.18.11. Trust flag `--auto`. Resume `--session
<id>`. Refusal pane text `Error: Session not found: <id>` (no new session row is
created). Readiness signature: the pane contains `Ask anything...`. Keys sent before it
are swallowed. Conversation ids are `ses_<...>`.
DECISION quota-reset: phases 8 and 15 burned on HTTP 402 (weekly premium-tier allowance),
not on code. The sanctioned seat change (`pipeline trial`) cannot run while the incumbent
arm is dead, so no seat was changed; the operator redeemed a Reset Pass instead. Run resumed
on the original seats.
DECISION host-swap: phase 4 burned 3 attempts, every one an OOM SIGKILL (3.8 GB RAM, no
swap, other sessions on the host). Operator approved a 4 GB /swapfile (persisted in
/etc/fstab) and `go test -p 1` in the Makefile; neither weakens a test. Run resumed.
DECISION publish-target: operator asked for the finished repo to be pushed to GitHub as
DevPassDiscordRelay under account syedmusaasad. A token is NOT needed: an SSH key for
github.com is already installed on this box (verified: `ssh -T git@github.com` greets
syedmusaasad). Publish therefore goes over SSH, and no credential of any kind enters
the gate sentinel (the engine journals the first 200 chars of sentinel content).
DECISION phase15-continue: operator said "continue" after both phase-15 reviewers
blocked on unproven session ownership, an isolated resume-refusal test failure, and
production 429 handling. Continue with verified binding discovery, then remediation
and cross review; no acceptance of these defects or relaxation of existing tests.
The earlier phase-14/15 proposal that pane-PID liveness plus row time proves row
ownership is withdrawn: the documented session schema has no process-writer field.
Phase 16 must demonstrate a supported real-CLI identity link before phase 15 resumes.
Phase 17 is a fresh review-acceptance checkpoint before phase 8, including when the
engine allows a split BLOCKING/CONCERNS verdict. Model seats remain unchanged.
DECISION pre-publish-fixes: operator chose "fix first" over publishing as-is. Phases 9 and
10 landed on split verdicts, but their blocking findings are real deployment and security
defects: relay accepts any non-bot author when DISCORD_USER_ID is unset (fail-open); the
installed user unit cannot find dpmux on the user manager's PATH; dpmux update overwrites
a running binary (ETXTBSY) and ignores a moved latest tag; telemetry sends only at
startup; docs overclaim (non-root, telemetry unmentioned, repo-root .env). Phase 23 fixes
all of them before the publish gate; phase 11 now depends on 23.
DECISION phase22-commit-reviewed-work: operator approved resuming phase 22 after both
reviewers blocked a03d977: the fixes they asked for (version-probe guard in the dpmux
marker stub, heartbeat consumer gate in mock-discord, RESUMED handling in relay, a
discriminating dispatch-before-READY test) exist only as uncommitted working-tree edits,
and REMEDIATION-22.md claims flake-free while recording an undiagnosed marker flake.
The retry commits the reviewed working tree, corrects the note, and must leave no
uncommitted relay changes.
DECISION readiness-gateway-remediation: operator approved phase 22 after phase 21 stopped
on unanimous BLOCKING. TestInject_ReadinessProbe is still intermittent: tests can dispatch
a gateway event before the relay has an IDENTIFY/READY consumer, so the event is lost.
Fix: relay exposes gateway readiness; the mock holds dispatches until a session is READY;
tests wait on readiness. EXIT logs move to per-run mktemp dirs (phase-21 reviewers had
overwritten a fixed /tmp log, which is why they misread REMEDIATION-21 as false). Phase
21 now re-verifies after 22; phase 9 after 21.
DECISION flaky-suite-remediation: operator approved phase 21 after phase 9 stopped on
unanimous BLOCKING: the engine saw `make test` green while three reviewer runs saw it red
(intermittent). Causes: pending marker not visible to the child on the systemd-run scope
birth path, a TestInject_ReadinessProbe timeout, and a restart-survival test that swaps
in a replacement unit when restart fails. EXITs now require the suite green three
consecutive times plus repeated runs of the marker tests on the forced scope path.
DECISION integrity-remediation: operator approved phase 20 after phase-19 reviewer-b found
test weakening (marker-ordering assertions dropped), evidence mismatch (ISOLATION-MATRIX
durations not matching logs), a no-op test cited as evidence, a non-discriminating ACK test,
an out-of-scope ASC->DESC change, and a "tm"+"ux" split evading the resolver grep; plus
phase-9 reviewer-a: dpmux update ignores the latest tag. The crude string grep is replaced
by an exec-call predicate. Phase 9 re-runs after phase 20.
DECISION m56-remediation: operator approved a single remediation phase for the
milestone-5/6 review findings after phase 9's unanimous BLOCKING stop
(2026-09-07). Covers: a real restart-survival test (no unconditional skip),
a correct host recipe, interaction ACKs, one-shot flag removal, component
throttle bypass, tmux-bin resolution, telemetry shape, isolation logging,
and dpmux update outside a checkout. Anti-vacuous EXIT pattern from phase 15
applied: filtered tests must show a real '--- PASS:' run.
DECISION review-timeout-2700: reviewer-b (grok-4-6) timed out at the fixed 900s
review cap THREE consecutive rounds (2026-09-07 20:21, 20:44, 21:06 UTC) while
actively working — a slow reviewer, not a hung one. Each timeout burned a phase
attempt despite reviewer-a PASS and all 13 EXITs green on the committed work
(78f881e). Resume carries PIPELINE_REVIEW_TIMEOUT_S=2700 in the engine
environment: an infrastructure knob, not a gate or seat change. The implementer
re-run on resume costs ~6 minutes because the work is already committed.
DECISION exit-predicate-pipefail: the three tee|grep-9 EXITs failed under the
engine's `bash -o pipefail` because grep -q exits on first match, SIGPIPE-ing
make/tee → nonzero pipeline, even though every run actually passed ('--- PASS:'
in the logs, no 'no tests to run', commit 14c70a5 landed). Rewritten as
redirect-then-grep, which is pipefail-safe. The work was never red; the
predicate was. Re-attempts must not redo or re-land already-committed work.
DECISION phase15-split: operator approved the recommended restructure after the
2026-09-05 20:12 burn. The phase was too large for one 3600s attempt (attempt 1
landed all buildable EXITs in 24 min; attempts 2/3 wrote the tests and production
code, then timed out in the Docker verification loop). Phase 15 now covers ONLY
implementation + tests + the named commit with the existing evidence predicates;
the five-run uncached isolation matrix moves to a new phase 18. Uncommitted
in-tree work from the timed-out attempts (tests, production changes, the
REMEDIATION-15.md skeleton) is the starting point, not a blank slate.
DECISION phase15-retry-2: operator approved one more retry with three amendments:
(a) the filtered-test EXITs must prove the named test actually ran and passed
('--- PASS:' evidence plus existence greps) because three predicates passed
vacuously with "no tests to run"; (b) an investigation budget of at most 15
minutes before the first file edit, with docs/REMEDIATION-15.md completed before
the final commit; (c) on record that the previous burn's attempts 2/3 were a
collateral SIGTERM sweep from a concurrent session stopping trial workers on this
host — environmental, not a task verdict. No scope, gate, or review changes.
DECISION phase15-provider-retry: operator said "continue" after the latest phase-15
attempt exhausted its budget with an aws-bedrock 403 saying anthropic.claude-sonnet-5
is not available for the account. This is not a 402 weekly-quota error. A short
journaled implementer preflight must demonstrate current tool access and validate
the plan and generated-agent consistency before resuming this run. Use the registry's
existing primary/family fallback; no model-seat, provider-config or gate changes are
authorized. Preserve unfinished code and all phase-15 EXITs and cross review. If
neither configured route can complete the preflight, remain stopped and report it.

## Phase 1: toolchain (fast-worker)
TIMEOUT: 1200
EXIT: /usr/local/go/bin/go version | grep -q 'go1.2'
EXIT: command -v make && command -v docker
EXIT: docker info >/dev/null 2>&1
EXIT: test -s docs/TOOLCHAIN.md

Install: Go from https://go.dev/dl/ (latest stable linux-amd64 tarball, verify sha256
from the download page's checksum, extract to /usr/local/go), and `apt-get install -y
make docker.io` (then `systemctl enable --now docker`). Add
`/etc/profile.d/golang.sh` exporting PATH=$PATH:/usr/local/go/bin. Write
docs/TOOLCHAIN.md: versions installed and the exact commands used. Idempotent: if a
tool is already present at an acceptable version, skip it and say so. Do not commit.

## Phase 2: transcript schema and prompt recipes (researcher)
AFTER:
TIMEOUT: 1800
EXIT: test -s docs/TRANSCRIPT-SCHEMA.md
EXIT: test -s testdata/fixtures/pong-session.json
EXIT: grep -q '^## Approval prompt' docs/TRANSCRIPT-SCHEMA.md
EXIT: grep -q '^## Question picker' docs/TRANSCRIPT-SCHEMA.md
EXIT: grep -q '^## Turn boundaries' docs/TRANSCRIPT-SCHEMA.md
EXIT: ! tmux ls 2>/dev/null | grep -q '^dp-discovery-'

Read docs/DISCOVERY.md first. Write docs/TRANSCRIPT-SCHEMA.md, the contract the DB
adapter and the fake CLI are both built to. Read the DB only via python3 sqlite3 with
URI `file:/root/.local/share/devpass-code/opencode.db?mode=ro`.

Required sections (exact headings):
`## Tables` — full `CREATE TABLE` text of session, message, part, session_message,
session_input and any table referencing a session id. Column meanings; which columns
carry timestamps (units) and monotonic ordering; where parent/subagent relation lives
(needed to ignore subagent sessions).
`## Event mapping` — for each spec event type (user message, assistant text, tool call,
tool result, turn complete) the exact row shape: role value, part type value, JSON
field names, with one verbatim example row each taken from the DB. How harness- or
agent-injected preamble/system parts appear in a user message (these are what invariant
2 must parse per PART).
`## Turn boundaries` — what marks assistant-turn completion (a column on message? a
part type? finish reason?), observed by polling the DB every 250 ms during a live pong
turn in a disposable tmux session; include the poll log showing rows appearing mid-turn.
`## Approval prompt` — run devpass-code in a disposable tmux session with
XDG_CONFIG_HOME=/tmp/devpass-code/discovery/xdg-config and XDG_DATA_HOME=
/tmp/devpass-code/discovery/xdg-data (copy auth.json into the data dir if needed; never
edit the real config), with a config whose permission for bash is "ask" and no
`--auto`. Prompt: "run `ls` using your shell tool". Capture the approval prompt
verbatim with its key hints, and the keystrokes that approve / deny. Then confirm
`--auto` suppresses it. Record how the pending approval appears (or not) in the DB.
`## Question picker` — with the same isolated config, prompt: "ask me to choose between
apple and pear using your question tool, then stop". Capture the picker verbatim, its
DB representation, and the keystroke recipe to select option 2. If no picker exists in
1.18.11, state that with the evidence.
`## Isolation` — confirm XDG_DATA_HOME relocates opencode.db (path observed). This is
what the test suite will rely on to never touch the real HOME.

Also write testdata/fixtures/pong-session.json: a JSON dump (all rows, all columns) of
one complete real session that contains a user prompt, a tool call+result, and
assistant text. Kill every dp-discovery-* tmux session before finishing. Do not
commit. Final lines: the two paths written.

## Phase 3: scaffold, fake CLI, mock Discord, harness (implementer)
AFTER: 1, 2
TIMEOUT: 3600
ATTEMPTS: 3
EXIT: test -f go.mod && test -x dpmux && test -f Makefile && test -f docker-compose.test.yml
EXIT: make unit
EXIT: make integration
EXIT: git log -1 --format=%s | grep -qxF 'scaffold: module, fake devpass-code, mock Discord, test harness'

PATH must include /usr/local/go/bin. `git init` if needed; .gitignore covers bin/, .env,
*.db*. Module `github.com/devpass/devpass-discord-relay`, CGO_ENABLED=0, deps:
github.com/bwmarrin/discordgo, modernc.org/sqlite.

Layout: cmd/relay (watcher), cmd/fake-devpass-code (test double), cmd/mock-discord
(test double), internal/transcript (DB adapter: read-only tail, per docs/
TRANSCRIPT-SCHEMA.md), internal/tmuxctl, internal/discord, internal/state, dpmux (bash),
test/integration (Go tests tagged `integration` run inside Docker), testdata/.

Fake devpass-code: mimics 1.18.11 to the letter of docs/DISCOVERY.md and
docs/TRANSCRIPT-SCHEMA.md: writes the same SQLite schema under $XDG_DATA_HOME, renders a
pane whose ready signature is `Ask anything...`, honors `--auto`, `--session <id>`
(refusal text `Error: Session not found: <id>`), reads a script of turns from
$FAKE_DP_SCRIPT (JSON), fault knobs via env: FAKE_DP_REFUSE_FIRST_RESUME=1,
FAKE_DP_READY_DELAY_MS, FAKE_DP_TRANSCRIPT_DELAY_MS (delay the session row),
FAKE_DP_SWALLOW_UNTIL_READY=1. Version-pin comment in its header.

Mock Discord: gateway websocket (HELLO/IDENTIFY/READY/heartbeat/dispatch MESSAGE_CREATE
and INTERACTION_CREATE) + REST (create message, edit, create thread, typing, upload,
rate-limit injection on demand) with a control HTTP endpoint tests use to inject events
and read everything the relay sent. discordgo must connect to it via overridable
endpoint vars in internal/discord.

Makefile targets: `build` (static binary to bin/relay), `unit` (`go test ./...`),
`integration` (`docker compose -f docker-compose.test.yml run --rm test`; honors
`T=<name>` → `-run`), `test` (= unit + integration). The integration image has tmux,
runs as a non-root user with a throwaway HOME, never mounts the developer's HOME.
Ship one passing smoke integration test (fake CLI starts in tmux, ready signature
appears, session row appears) so the harness is proven. Commit exactly:
`scaffold: module, fake devpass-code, mock Discord, test harness`.

## Phase 4: milestone 1, mirror only (implementer)
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestInvariant2_FirstSightReplay' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm1: mirror terminal sessions into Discord threads'

dpmux: `dpmux [args]` → session `dp-<unixtime>-<pid>` on the default per-uid socket,
passes `--auto` unless `--untrusted` appears anywhere in args; writes
~/.devpass-discord-relay/pending-<session-name> containing the tmux target before
spawning; inside a pane on the same default socket → new-window in the host session,
target `<host-session>:=<window-name>`; other socket → fresh default-socket session.
`dpmux ls` (suppress window lines duplicating their session name). Relay binary path,
DB path, state dir all overridable by env (RELAY_DEVPASS_DB, RELAY_STATE_DIR).

Watcher: config from .env resolved $DEVPASS_RELAY_ENV → ~/.config/devpass-discord-relay/
.env → ./.env. Polls the DB ~1s for new session rows; pairs with pending markers
(unmatched sessions ignored, subagent sessions ignored); creates one thread per session
in the configured channel; tails message/part rows; streams assistant text via
throttled edits (≤5 edits/5s per channel, 2000-char chunking at natural boundaries,
429 backoff); echoes terminal-typed user prompts truncated to 300 chars; one tool
embed updated in place (rows flip done/failed); typing loop re-armed under 10s;
"turn finished without output" line when a turn ends with no text; tool error then
clean turn end softened in place; muted summary line (duration · tool count).
Liveness polls the full stored target; on death post a session-ended marker, flip the
thread-root status emoji, keep the sessions.json claim. sessions.json in the state dir.
First-sight replay: a completed first exchange already in the DB at attach still
mirrors; preamble parts are classified per PART, never on joined text.

Tests: unit for chunking, throttle, part classification (a preamble-bearing user
message must not count as a second prompt); integration `TestInvariant2_FirstSightReplay`
(fake CLI completes a full turn before the relay starts; the thread shows it) plus
mirror tests for the embed flip, no-output line, and softening. Commit exactly:
`m1: mirror terminal sessions into Discord threads`.

## Phase 5: milestone 2, inject into a live session (implementer)
TIMEOUT: 2400
ATTEMPTS: 3
EXIT: make test
EXIT: grep -rq 'func TestInject_ReadinessProbe' test/integration
EXIT: grep -rq 'func TestInject_ForeignAuthorDropped' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm2: inject Discord thread replies into the live pane'

Thread reply → readiness probe (capture-pane for `Ask anything...` up to 30s, else
fixed 2s fallback) → `tmux send-keys -t <target> -l <text>` then Enter. Only
DISCORD_USER_ID's messages are honored; all other authors including bots dropped in
the handler before any work. `!title <t>` renames the thread. Image attachments
download to <state>/staging/<session-id>/ with filenames sanitized of exotic whitespace;
the local path is appended to the injected text. Agent-emitted image parts upload to the
thread. Tests: `TestInject_ReadinessProbe` (FAKE_DP_SWALLOW_UNTIL_READY=1 +
FAKE_DP_READY_DELAY_MS=3000; the prompt still lands exactly once),
`TestInject_ForeignAuthorDropped`, an attachment path test. Commit exactly:
`m2: inject Discord thread replies into the live pane`.

## Phase 6: milestone 3, spawn from Discord (implementer)
AFTER: 13
TIMEOUT: 2400
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestInvariant1_OwnTranscriptBinding' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm3: spawn a new session from a Discord message'

Message in the relay channel outside any thread → spawn `dpmux` (systemd-run wrapping
comes in Phase 9; leave a TODO comment naming Phase 9) → wait for a session row whose
creation time is after the spawn's start instant AND matching this spawn's pending
marker → bind → inject as first prompt. Never glob/lexicographic; never a
process-start baseline snapshot. Test `TestInvariant1_OwnTranscriptBinding`: a stale
session row whose id sorts first and was created after relay start but before the
spawn must lose to the genuinely new one. Commit exactly:
`m3: spawn a new session from a Discord message`.

## Phase 7: milestone 4, resume and refusal handling (implementer)
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: make integration T=TestInvariant4_AmbiguousIdCandidateList && make integration T=TestInvariant4_AmbiguousIdCandidateList
EXIT: grep -rq 'func TestInvariant3_InjectionEchoVerification' test/integration
EXIT: grep -rq 'func TestInvariant4_ResumeRefusalRetry' test/integration
EXIT: grep -rq 'func TestInvariant4_AmbiguousIdCandidateList' test/integration
EXIT: grep -rq 'FAKE_DP_SWALLOW_UNTIL_READY' test/integration/resume_test.go
EXIT: ! grep -q 'kill-session' internal/mirror/watcher.go
EXIT: git log -1 --format=%s | grep -qxF 'm4-fix: exact teardown, refusal baseline, echo dedup, provider in candidates'

STATE: commit 0d3cdf0 `m4: resume dead sessions, retry refusals, verify injection echo`
already landed the first cut of this milestone and BOTH cross reviewers returned
BLOCKING. Read both verdicts first:
/root/.system/runs/run_20260904T054447_438b65/phase-7/review/reviewer-a/review.md and
.../reviewer-b/review.md. This attempt is the remediation; the spec text for the
milestone is unchanged: reply in a dead thread → re-spawn `--session <id>`, inject after
readiness, watch the DB ≤30s for the echo, on absence re-send the identical keystrokes
exactly once and suppress the duplicate echo in the thread; refusal by BOTH signals (pane
`Error: Session not found:` OR a new session row with an id other than requested); on
refusal teardown exactly like `!quit`, retry once silently, second failure → one explicit
notice, never silently bind to the wrong id; ambiguous id → numbered candidates (provider
+ last-active age) and wait for `!resume <n>`; `!quit` tears down but keeps the claim.

Fix every finding, each with evidence in a test:
1. internal/mirror/watcher.go teardownPane: `kill -TERM` the pane's process group
   (`tmux display -p -t <target> '#{pane_pid}'`, then kill -TERM -<pgid>), wait briefly,
   then `tmux kill-window -t <target>`. Never kill-session (the target may live in the
   user's host session). A test that runs teardown on a window inside a two-window
   session and asserts the other window survives.
2. Refusal detection: take the session-table baseline BEFORE spawnResume starts, and do
   not declare success on pane readiness alone; success requires the pane to be ready AND
   no new session row with a foreign id since the baseline. A test where the fake creates
   a foreign id with no pane refusal text must be detected as refusal.
3. Duplicate echo suppression: after a resend, the second user-prompt row with identical
   text within the verification window is not posted to the thread. Rewrite
   TestInvariant3_InjectionEchoVerification to set FAKE_DP_SWALLOW_UNTIL_READY=1 with a
   ready delay, and assert (a) the prompt is present in the DB exactly once after the
   retry or, if the fake echoes twice, (b) the thread shows the echo exactly once.
4. Candidate list rows include the provider (from the session/message provider column
   documented in docs/TRANSCRIPT-SCHEMA.md) and last-active age.
5. pendingResumeStore.handleResumeChoice is a stub; implement it or delete it and route
   `!resume <n>` for untracked threads through the real handler.
6. TestInvariant4_AmbiguousIdCandidateList is flaky/timing out: two pending markers
   pointing at nonexistent tmux targets race in findPendingMarker. Give each candidate
   its own real (fake-CLI) tmux target or construct the ambiguity through the DB alone
   with sessions.json records, and make the mock gateway delivery deterministic (wait for
   the relay's READY ack before dispatching `!resume`). The EXIT runs it twice in a row.
Commit exactly (stage only the files you changed):
`m4-fix: exact teardown, refusal baseline, echo dedup, provider in candidates`.

## Phase 8: milestone 5, approvals and questions (implementer)
AFTER: 17
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestApproval_AutoDenyTimeout' test/integration
EXIT: grep -rq 'func TestInvariant5_RecipesVersionPinned' internal
EXIT: grep -rq 'verified against devpass-code 1.18.11' internal/tmuxctl
EXIT: git log -1 --format=%s | grep -qxF 'm5: approval buttons, question menus, auto-deny timeout'

Pane-detected approval prompt (text per docs/TRANSCRIPT-SCHEMA.md §Approval prompt) →
Discord message with Approve/Deny buttons; click drives the pinned keystrokes; unanswered
→ auto-deny after APPROVAL_TIMEOUT_SECONDS (default 300) and the message is edited to
say so. Question pickers (if §Question picker found one; otherwise implement against
the fake CLI's picker and mark the recipe `unverified against real TUI` in the same
comment) → select menu. Every keystroke recipe lives in internal/tmuxctl/recipes.go,
each with a comment containing `verified against devpass-code 1.18.11`;
`TestInvariant5_RecipesVersionPinned` (unit) parses that file and fails if any recipe
lacks the pin. Commit exactly:
`m5: approval buttons, question menus, auto-deny timeout`.

## Phase 9: milestone 6, restart survival, install, lifecycle (implementer)
AFTER: 21
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestRestartSurvival_DisposableUnit' test/integration
EXIT: grep -rq 'func TestTTL_NeverSweepsLiveWindow' internal/state
EXIT: bash -n install.sh && bash -n dpmux && test -s systemd/devpass-discord-relay.service
EXIT: grep -q '%h' systemd/devpass-discord-relay.service
EXIT: git log -1 --format=%s | grep -qxF 'm6: restart survival, TTL sweep, telemetry, installer, unit file'

Wrap every tmux invocation that may birth the server (new-session, new-window when
no server) in `systemd-run --user --scope --collect`; read-only commands unwrapped;
log the isolation mode at startup; clean fallback when systemd-run or the user manager
is absent. Binary resolution: RELAY_TMUX_BIN → /usr/local/bin/tmux if executable and
`-V` succeeds → PATH; never bare PATH-first; same pattern for devpass-code
(RELAY_DEVPASS_BIN). `TestRestartSurvival_DisposableUnit`: inside the integration
container (or skipped with an explicit reason if no user manager is reachable, and then
ALSO provided as `make restart-survival-host` running against a disposable
`systemd-run --user --unit relay-test-<pid>` on the host with an isolated
XDG_DATA_HOME): start relay as a unit, spawn a session, restart the unit, the tmux
window is still alive. sessions.json TTL sweep at boot and every 24h,
SESSION_TTL_DAYS default 30, never removes a record whose window is alive
(`TestTTL_NeverSweepsLiveWindow`). Telemetry: daily POST {sha256(username)[:12],
version} to TELEMETRY_URL if set; quiet on failure; nothing else.
install.sh: idempotent (done steps exit success), builds, installs binary + dpmux to
~/.local/bin, unit to ~/.config/systemd/user/ using %h, backs up and restores .env,
enables the service. `dpmux update` (git pull to tag `latest`, rebuild, prompt to
restart) and `dpmux uninstall` (backs up .env). Commit exactly:
`m6: restart survival, TTL sweep, telemetry, installer, unit file`.

## Phase 10: operator documents (document-writer)
TIMEOUT: 1800
SURFACE: README.md operator-doc
SURFACE: SETUP.md operator-doc
REVIEW: cross
EXIT: test -s README.md && test -s SETUP.md && test -s CONTRIBUTING.md && test -s CHANGELOG.md
EXIT: grep -q 'capture-pane' README.md && grep -q 'opencode.db' README.md
EXIT: grep -qi 'MESSAGE CONTENT' SETUP.md
EXIT: grep -q 'make test' CONTRIBUTING.md && grep -q 'T=' CONTRIBUTING.md
EXIT: git log -1 --format=%s | grep -qxF 'docs: README, SETUP, CONTRIBUTING, CHANGELOG'

Sources: the code, docs/DISCOVERY.md, docs/TRANSCRIPT-SCHEMA.md, the plan preamble
DECISIONs. README.md: one-paragraph philosophy (tmux session is the source of truth,
Discord is mirror + remote), an ASCII architecture diagram, an explicit
"Transcript vs pane" section (DB-tailed content vs capture-pane state-only, and why the
transcript is opencode.db on this version), the Discord structural difference (bots
cannot thread in DMs → your own private guild, privacy consequence), and a "Security
posture" section with the spec's points verbatim in substance: runs on your machine
under your account, per-user token in .env never committed and backed up across
reinstalls, single-user gating at the event layer, outbound only, content goes only to
your own private guild. SETUP.md: developer-portal walkthrough (application, bot,
token, MESSAGE CONTENT intent, invite to your guild, channel id, user id, .env keys).
CONTRIBUTING.md: `make unit`, `make integration`, `make test`, `T=<name>` filter, the
fake CLI knobs, the mock Discord control endpoint, the rule that tests never touch the
real HOME or real Discord. CHANGELOG.md: 0.1.0 with the six milestones. Stage only these
four files; commit exactly: `docs: README, SETUP, CONTRIBUTING, CHANGELOG`.

## Phase 11: ready to publish (gate)
AFTER: 23
GATE: .gate-publish

## Phase 12: publish to GitHub (implementer)
TIMEOUT: 900
EXIT: git remote get-url origin | grep -qE 'github.com:syedmusaasad/DevPassDiscordRelay(\.git)?$'
EXIT: git fetch -q origin && git diff --quiet HEAD origin/main
EXIT: git ls-remote --tags origin | grep -q 'refs/tags/latest$'

Publish over SSH — the box already carries a working key for github.com
(DECISION publish-target; `ssh -T git@github.com` greets syedmusaasad). The gate
sentinel needs NO credential content: write only the word `publish` to it; never
put any secret in the sentinel (the engine journals its first 200 chars). Steps:
Confirm the working tree is clean and `make unit` passes before pushing. The
repository must exist first: check `git ls-remote
git@github.com:syedmusaasad/DevPassDiscordRelay.git`; if it does not (fresh
remote), STOP and report — repo creation needs the GitHub UI or API from the
operator side; do not attempt token-based creation. Set
`git remote add origin git@github.com:syedmusaasad/DevPassDiscordRelay.git`
(or set-url if origin exists), ensure the branch is named `main`, push with
`GIT_SSH_COMMAND='ssh -o BatchMode=yes' git push -u origin main`, then tag HEAD
`latest` (this is what `dpmux update` pulls) and push the tag
(`git push origin latest`). Never echo or commit any credential. Final lines:
`git ls-remote --tags origin`.

## Phase 13: m1 review remediation (implementer)
AFTER: 5
TIMEOUT: 2400
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestMirror_LongTextChunksToFollowups' test/integration
EXIT: grep -rq 'func TestMirror_ToolErrorSoftenedOnCleanEnd' test/integration
EXIT: grep -rq 'func TestThrottle_429Backoff' internal
EXIT: grep -rq 'func TestDpmux_MarkerBeforeSpawn' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm1-fix: chunk long text, soften tool errors, honor 429, marker before spawn'

Cross review of phase 4 (verdicts in /root/.system/runs/run_20260904T054447_438b65/
phase-4/review/reviewer-a/review.md and reviewer-b/review.md; read both) found spec gaps
that landed. Fix each, with the named test:
1. internal/mirror/watcher.go truncates assistant text to one 2000-char chunk. Post ALL
   chunks: first message edited in place while streaming; when it would exceed 2000,
   split at the last paragraph/sentence/space boundary and continue in a follow-up
   message that becomes the new edit target. `TestMirror_LongTextChunksToFollowups`
   (fake CLI emits a 5000-char reply; the thread holds >=3 messages whose concatenation
   equals the reply).
2. Tool error followed by a clean step-finish is never softened. Implement in-place:
   error row flips to a warning glyph and the embed footer reads "turn completed".
   `TestMirror_ToolErrorSoftenedOnCleanEnd`.
3. 429 from Discord is treated as an ordinary error. Parse retry_after from the
   discordgo RateLimitError / response body, sleep it, retry the same edit/send once;
   feed the throttle. `TestThrottle_429Backoff` (unit, mock returning one 429 then 200).
4. cmd/dpmux writes pending-<name> AFTER tmux new-session. Write it BEFORE spawning
   (the brief and the spec say so). `TestDpmux_MarkerBeforeSpawn` (integration: run
   dpmux with RELAY_DEVPASS_BIN pointing at a stub that records whether the marker
   existed at its start).
5. cmd/dpmux inside-pane detection: only use new-window when $TMUX names the default
   per-uid socket (/tmp/tmux-<uid>/default or $TMUX_TMPDIR equivalent); any other
   socket → fresh session on the default socket.
6. Liveness must check the full target: `tmux list-panes -t <target>` (window-level),
   not `has-session`.
7. `dpmux ls`: list only dp-* sessions and their windows, suppressing window lines that
   duplicate their session name. Drop the `_ = strconv.Itoa` hack.
Do not touch phase-5 (inject) behavior beyond what these fixes require. Commit exactly:
`m1-fix: chunk long text, soften tool errors, honor 429, marker before spawn`.

## Phase 14: spawn-binding and dpmux hardening from reviews (implementer)
AFTER: 7
TIMEOUT: 2400
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestInvariant1_OwnTranscriptBinding' test/integration
EXIT: grep -rq 'func TestDpmux_NewWindowMarkerBeforeSpawn' test/integration
EXIT: grep -rq 'func TestDpmux_ForeignSocketFallsBackToDefault' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm3-fix: prove marker-to-session binding, dpmux socket and marker order'

Cross reviews of phases 6 and 13 left findings that were not remediated (read
/root/.system/runs/run_20260904T054447_438b65/phase-6/review/reviewer-*/review.md and
phase-13/review/reviewer-*/review.md). Fix:
1. Spawn binding (internal/mirror/watcher.go, spawn binder and findPendingMarker): a
   Discord spawn binds only to a session row that is provably ITS OWN. Make the link
   concrete: dpmux writes into the pending marker a nonce and its exact spawn instant,
   and passes the nonce to devpass-code's environment or working directory so it is
   recoverable from the session row (docs/TRANSCRIPT-SCHEMA.md: the session table's
   directory/title columns — pick whichever the fake and the real CLI both persist; if
   nothing is recoverable, bind by strict ordering: row time_created >= marker time AND
   the row's pane pid is a descendant of the marker's tmux pane, checked via
   `tmux display -p -t <target> '#{pane_pid}'` and /proc). Never accept rows created
   before the marker; remove the 5-second grace. Strengthen
   TestInvariant1_OwnTranscriptBinding to the brief's exact scenario: a stale row created
   AFTER relay start but BEFORE the spawn, with an id that sorts first, must lose.
2. cmd/dpmux: write the pending marker BEFORE `tmux new-window` on the inside-pane path
   too (`TestDpmux_NewWindowMarkerBeforeSpawn`); when $TMUX names a socket other than
   the default per-uid socket, unset TMUX and run `tmux -L default new-session` so the
   fresh session lands on the default socket (`TestDpmux_ForeignSocketFallsBackToDefault`,
   using a second tmux server on `-L relaytest`).
3. 429 handling: disable discordgo's internal retry (`Session.MaxRestRetries = 0` or
   equivalent), parse `*discordgo.RateLimitError` (RetryAfter) in the relay's send/edit
   path, sleep, retry once, feed the throttle; update TestThrottle_429Backoff to use the
   typed error. Delete the dead `rateLimitErr`, exported `ExtractRetryAfter`, and unused
   `chunkedBytes`.
Commit exactly: `m3-fix: prove marker-to-session binding, dpmux socket and marker order`.

## Phase 15: m3-fix review remediation (implementer)
AFTER: 16
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: make integration T=TestInvariant4_ResumeRefusalRetry && make integration T=TestInvariant4_ResumeRefusalRetry
EXIT: grep -rq 'discordgo.RateLimitError' internal/mirror/throttle429_test.go
EXIT: test ! -e relay && test ! -e mock-discord && test ! -e fake-devpass-code && test ! -e integration.test
EXIT: grep -qx 'BINDING_PROTOCOL: VERIFIED' docs/BINDING-PROTOCOL.md
EXIT: grep -rq 'func TestInvariant1_ConcurrentForeignSessionRejected' test/integration
EXIT: grep -rq 'func TestInvariant1_MissingIdentityFailsClosed' test/integration
EXIT: grep -rq 'func TestDiscord429_ProductionClientBackoff' test/integration
EXIT: make integration T=TestInvariant1_ConcurrentForeignSessionRejected > /tmp/p15-c.log 2>&1; grep -q -- '--- PASS: TestInvariant1_ConcurrentForeignSessionRejected' /tmp/p15-c.log && ! grep -q 'no tests to run' /tmp/p15-c.log
EXIT: make integration T=TestInvariant1_MissingIdentityFailsClosed > /tmp/p15-m.log 2>&1; grep -q -- '--- PASS: TestInvariant1_MissingIdentityFailsClosed' /tmp/p15-m.log && ! grep -q 'no tests to run' /tmp/p15-m.log
EXIT: make integration T=TestDiscord429_ProductionClientBackoff > /tmp/p15-d.log 2>&1; grep -q -- '--- PASS: TestDiscord429_ProductionClientBackoff' /tmp/p15-d.log && ! grep -q 'no tests to run' /tmp/p15-d.log
EXIT: test -s docs/REMEDIATION-15.md && ! grep -q 'PENDING' docs/REMEDIATION-15.md
EXIT: git log -1 --format=%s | grep -qxF 'fix: verified spawn identity, isolated resume retry, production 429 backoff'

STATE: the previous m3-fix2 implementation exists; both phase-15 reviewers blocked it.
Read these exact findings and the phase-16 evidence before editing application code:
/root/.system/runs/run_20260904T054447_438b65/phase-15/review/reviewer-a/review.md
/root/.system/runs/run_20260904T054447_438b65/phase-15/review/reviewer-b/review.md
docs/BINDING-PROTOCOL.md and testdata/fixtures/binding-protocol.json.

RECOVERY CONTEXT: the most recent attempt-3 worker returned exit 1 after real work
with an aws-bedrock 403 for claude-sonnet-5. Source receipt:
/root/.system/runs/run_20260904T054447_438b65/phase-15/attempt-3/try-0/result.json.
On this retry first inventory git status/diff and existing phase-15 code/tests; do not
discard or reimplement partial work wholesale. The preflight only proves dispatch
availability, not test success or resolution of the three review findings. Write
docs/REMEDIATION-15.md early as an honest checkpoint, with unfinished checks explicitly
pending, then replace those entries with observed results before the named commit.
All verification predicates and review/acceptance requirements below remain in force.

PREVIOUS-RETRY POSTMORTEM (journal, 2026-09-05 19:10 UTC): attempt 1 spent 55 minutes
on investigation only — reproducing the resume-refusal test and probing --print-logs —
and exited 0 with NO code written, NO docs/REMEDIATION-15.md, and NO commit, so the
remediation-doc and commit-subject EXITs failed. Attempts 2 and 3 were killed ~10s in
by a collateral SIGTERM sweep from a concurrent session stopping trial workers on this
host (same 19:10 window; environmental). Also: three filtered-test EXITs passed
VACUOUSLY ("no tests to run") because the named tests did not exist; the EXITs above
now demand the test exists AND shows a '--- PASS:' line. Working rules for this
retry: spend at most 15 minutes investigating before the first file edit; write
docs/REMEDIATION-15.md progressively (mark unfinished checks as pending) and complete
it before the commit; the binding protocol (stderr 'message=created id=ses_<ID>' per
unique pane log) is verified — build the three missing regression tests against it
first, then fix the production 429 path.

Scope: the three blocking findings only, their regression tests, required fixture/mock
changes, and docs/REMEDIATION-15.md. Preserve previously landed cleanup and all named
invariants. No milestone-5 features, installed CLI changes, registry changes, real
Discord calls, or writes to the user's real HOME/data/config in tests.

1. Replace the unsafe bindSessionByPanePid/first-new-row fallback with the exact identity
   protocol actually demonstrated in phase 16. Neither /proc existence, parentage of a
   CLI process, row timestamps, a title nonce only the fake emits, nor a shared directory
   proves who wrote a session row. Those may constrain a verified identity but cannot
   replace it. Preserve the real TUI, original working directory, read-only DB content
   source, and existing conversation history. A missing or ambiguous identity fails
   closed: no claim, mirror, or injection into an arbitrary row. Both background pending
   marker attachment and Discord-triggered binding must use the same evidence and
   atomically claim each marker/session only once. Check target liveness at the actual
   bind, not just entry, and preserve exact historical-id binding on resume.
   Required regressions:
   - TestInvariant1_OwnTranscriptBinding: stale row created AFTER relay start and BEFORE
     this spawn, sorts first, loses to the actual new session.
   - TestInvariant1_ConcurrentForeignSessionRejected: unrelated top-level row appears
     AFTER spawn starts and before its delayed own row, including in the same directory;
     only the actual own session is bound and receives the queued prompt.
   - TestInvariant1_MissingIdentityFailsClosed: a live pane plus eligible foreign rows,
     absent/invalid identity evidence (and a pane dying during the wait), never binds or
     receives injected text. Preserve the old stale-row checks even if helpers change.
   The fake must reproduce the verified real protocol, not invent an environment-to-title
   behavior. Fixtures cite the real CLI version and phase-16 observation.

2. Reproduce TestInvariant4_ResumeRefusalRetry in a fresh, isolated test environment
   with no existing tmux server. Reviewers disagree on repeatability; capture the precise
   commands, exit codes and stderr. Remove reliance on earlier tests' servers and stop
   hiding tmux launch failures (`2>/dev/null || true` in the fake-dpmux wrapper is suspect).
   Wait for actual startup/teardown and gateway readiness; all components in each test
   use its own default-socket namespace (e.g. inherited TMUX_TMPDIR) and cleanup only
   that namespace. Keep the first-refusal fault and assert exactly two resume launches,
   one successful injection, same requested id, and unrelated host windows surviving.
   Do not add a keepalive window solely to conceal the missing-server bug, increase
   timeouts to hide it, or skip the test. The single-run filtered EXIT (twice in a row)
   stays in THIS phase; the five-run uncached matrix is phase 18's, not yours — do not
   run it here. Build the current image before verification; an old Docker image or Go
   cache is not passing evidence.

3. Audit the actual Discord client constructed by cmd/relay/main.go as well as
   internal/discord/discord.go. Configure ShouldRetryOnRateLimit=false on production
   sessions so real *discordgo.RateLimitError responses reach the relay's bounded,
   context-cancellable retry and channel-throttle code. MaxRestRetries alone does not
   disable discordgo's 429 loop. Preserve RetryAfter (including fractional values),
   channel limits and existing bounded retry behavior. Keep TestThrottle_429Backoff;
   TestDiscord429_ProductionClientBackoff must use the real production client constructor
   against local mock REST that returns a real 429 JSON body and then 200. Assert the
   error reaches the relay throttle, the retry respects RetryAfter, request count is
   bounded and no duplicate message appears. A custom error stub alone is insufficient.

Write docs/REMEDIATION-15.md mapping each review finding to implementation and test
evidence, recording exact command outputs/status and remaining limitations — every
PENDING entry must be resolved (the five-run matrix is the one item that may cite
phase 18 as its follow-up instead of results). For cross review: independently
exercise the named filtered tests using fresh containers without a pre-existing
tmux server; report any remaining ownership or test failure as BLOCKING. The
operator checkpoint after review is not permission to leave a blocker unfixed.

Ceremony: a NEW commit, not amendment of the earlier m3-fix2 commit. Stage only the
relay implementation/test/doc files changed for these findings and the phase-16
evidence. Do not stage plans/001-build/plan.md, credentials, generated binaries, or
unrelated work. Commit exactly:
`fix: verified spawn identity, isolated resume retry, production 429 backoff`.

## Phase 16: establish a real session identity protocol (researcher)
AFTER: 14
TIMEOUT: 1800
ATTEMPTS: 1
EXIT: test -s docs/BINDING-PROTOCOL.md && test -s testdata/fixtures/binding-protocol.json
EXIT: grep -qx 'BINDING_PROTOCOL: VERIFIED' docs/BINDING-PROTOCOL.md
EXIT: python3 -c 'import json; d=json.load(open("testdata/fixtures/binding-protocol.json")); assert d["verified"] is True; assert d["cli_version"] and d["mechanism"] and d["observations"] and d["reproduction_commands"]'

The schema in docs/TRANSCRIPT-SCHEMA.md contains no writer PID. Previous phase-15
attempts repeatedly confused pane liveness with row ownership. Establish a genuine
per-launch link from the real devpass-code TUI process to its conversation id; do not
write another implementation using PID-plus-time inference.

Read docs/DISCOVERY.md, docs/TRANSCRIPT-SCHEMA.md, cmd/dpmux/main.go, the pending-marker
and spawn/resume paths in internal/mirror/watcher.go, and both current phase-15 review
files under /root/.system/runs/run_20260904T054447_438b65/phase-15/review/. Inspect
the installed /usr/local/bin/devpass-code version, supported CLI help and locally
available API/event/source documentation. Supported startup/session-id metadata may
establish identity; transcript content must still come only from the read-only DB.
Do not propose scraping content or the title/status line of the TUI for identity.

Demonstrate the smallest supported identity mechanism with the REAL installed CLI in
a disposable tmux TUI; headless `run --format json` behavior alone is not TUI evidence.
Use only /tmp/devpass-code/relay-binding-discovery for experiments, with HOME,
XDG_DATA_HOME, XDG_CONFIG_HOME, XDG_CACHE_HOME and TMUX_TMPDIR explicitly set beneath
it, TMUX cleared, and cwd a disposable project there. No real user auth/config/database
copies, credentials, Discord or real-HOME CLI invocation. Prefer startup-only/local
mock-provider experiments with no paid model calls. Scratch probe assets are allowed;
application code and installed config changes are not. Cleanup only experiment-owned
processes, panes and sockets. Record cleanup evidence.

Evidence must identify: (a) exact real CLI-supported mechanism and source, (b) exact
session id delivered from the launched TUI instance, (c) matching read-only DB row,
(d) working directory and history preserved, and (e) concurrent/unrelated-session
disambiguation, including a foreign row created after launch. Record version, commands,
relevant metadata outputs (no conversation content or credentials), and sandbox paths
in docs/BINDING-PROTOCOL.md and testdata/fixtures/binding-protocol.json. Any fixture is
observed, not fabricated to support the proposed mechanism. Explain how the same link
applies to dpmux terminal launch, Discord spawn and exact-id resume.

Scope constraints: preserve one static Go relay binary plus dpmux, the user's existing
session history/database, normal project cwd and no hosted/listening relay service.
Do not silently switch to per-session databases, alter CLI installations, install a
plugin into the user's config, or relocate project directories to manufacture identity.
If a new installed plugin, API control plane, changed cwd/storage model, credentials
or another unapproved architectural change is essential, write
`BINDING_PROTOCOL: BLOCKED` and the narrow blocker/options and stop. An unverified
or unavailable mechanism must not receive VERIFIED just to satisfy EXIT. The failed
EXIT intentionally stops the plan for a decision; no substitute on timestamps.

Write `BINDING_PROTOCOL: VERIFIED` only after an observed supported path meets these
conditions. JSON keys: verified (boolean), cli_version, mechanism, observations,
reproduction_commands. Do not commit. Final lines: the two output paths and verdict.

## Phase 17: accept corrected binding and lifecycle review (gate)
AFTER: 18
GATE: .gate-phase15-acceptance

Pause before milestone 5 for the operator to inspect the phase-15 cross reviews,
docs/REMEDIATION-15.md and docs/ISOLATION-MATRIX.md. This is a fresh sentinel,
separate from the publication gate; no worker writes it. An existing BLOCKING
verdict or red/uncorroborated test run requires remediation, not acceptance
merely because the engine permits split reviews.

## Phase 18: five-run uncached isolation matrix (implementer)
AFTER: 15
TIMEOUT: 3600
ATTEMPTS: 3
EXIT: test -f docs/ISOLATION-MATRIX.md
EXIT: grep -c '^RUN [0-9]*/5: PASS' docs/ISOLATION-MATRIX.md | grep -q '^5$'
EXIT: ! grep -qE '^RUN [0-9]*/5: FAIL' docs/ISOLATION-MATRIX.md
EXIT: grep -q 'uncached' docs/ISOLATION-MATRIX.md
EXIT: grep -qE 'no (pre-existing|existing) tmux server' docs/ISOLATION-MATRIX.md

The isolation matrix split out of phase 15 (DECISION phase15-split). Prove
TestInvariant4_ResumeRefusalRetry is repeatably green with no environmental
crutch. Run it FIVE times, each in a freshly built environment: rebuild the
integration image (`docker compose -f docker-compose.test.yml build --no-cache`)
before the runs or as needed to honor 'uncached', and pass `-count=1` so no Go
test cache is reused. Each run must start with no pre-existing tmux server in
its namespace; capture per run the exact command, exit code, and pass/fail of
the named test into docs/ISOLATION-MATRIX.md in the form `RUN <n>/5: PASS` (or
FAIL). Do not modify application code, tests, or docs/REMEDIATION-15.md in this
phase: this is verification only. If a run fails, record the failure verbatim
with its output and stop — a FAIL is evidence, not something to retry away; the
phase must not end green on a failing run. If you find a genuine environmental
flake, record it with the exact output and mark the phase failed honestly.
Final lines: the path written and the five RUN lines.

## Phase 19: milestone 5/6 review remediation (implementer)
AFTER: 17
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestInteraction_Ack' test/integration
EXIT: make integration T=TestInteraction_Ack > /tmp/p19-a.log 2>&1; grep -q -- '--- PASS: TestInteraction_Ack' /tmp/p19-a.log && ! grep -q 'no tests to run' /tmp/p19-a.log
EXIT: grep -rq 'func TestApproval_SecondPromptGetsButtons' test/integration
EXIT: make integration T=TestApproval_SecondPromptGetsButtons > /tmp/p19-b.log 2>&1; grep -q -- '--- PASS: TestApproval_SecondPromptGetsButtons' /tmp/p19-b.log && ! grep -q 'no tests to run' /tmp/p19-b.log
EXIT: grep -rq 'func TestRestartSurvival_DisposableUnit' test/integration
EXIT: make integration T=TestRestartSurvival_DisposableUnit > /tmp/p19-r.log 2>&1; grep -q -- '--- PASS: TestRestartSurvival_DisposableUnit' /tmp/p19-r.log && ! grep -q 'no tests to run' /tmp/p19-r.log
EXIT: make integration T=TestRestartSurvival_DisposableUnit > /tmp/p19-r2.log 2>&1; grep -q -- '--- PASS: TestRestartSurvival_DisposableUnit' /tmp/p19-r2.log && ! grep -q 'no tests to run' /tmp/p19-r2.log
EXIT: grep -q 'sha256' internal/telemetry/telemetry.go && grep -E 'username|digest' internal/telemetry/telemetry.go | grep -qE '12|:12'
EXIT: ! grep -rn '"tmux"' internal/ cmd/ --include='*.go' | grep -v '_test.go' | grep -qv 'tmuxBin\|resolveTmux\|RELAY_TMUX'
EXIT: test -s docs/REMEDIATION-19.md && ! grep -q 'PENDING' docs/REMEDIATION-19.md
EXIT: git log -1 --format=%s | grep -qxF 'fix: real restart survival, interaction acks, update path, telemetry shape'

STATE: commits 8258744 (m5) and be88961 (m6) landed; phase 9 cross review was
unanimously BLOCKING. Read BOTH review files first — every finding below maps to
one; do not redesign beyond them:
/root/.system/runs/run_20260904T054447_438b65/phase-9/review/reviewer-a/review.md
/root/.system/runs/run_20260904T054447_438b65/phase-9/review/reviewer-b/review.md
Phase-8 (m5) findings are in phase-8/review/reviewer-a/review.md (BLOCKING) and
phase-8/review/reviewer-b/review.md (CONCERNS) — the CONCERNS items overlap with
phase-9's and are covered here. Scope: these findings, their tests, and
docs/REMEDIATION-19.md only. No new features, no gate/seat changes, never touch
the real HOME or real Discord.

1. RESTART SURVIVAL (the unanimous blocker). The named test must actually run:
   in the integration container, skip ONLY when no user manager is reachable
   (probe and log the probe result; a skip must carry the probe failure text).
   When reachable: start the relay under a disposable
   `systemd-run --user --unit relay-test-<pid>` with isolated XDG_DATA_HOME and
   TMUX_TMPDIR, spawn a session through the relay's own spawn path (dpmux +
   pending marker, not a bare tmux), confirm the window is alive, then
   `systemctl --user restart relay-test-<pid>` (fall back to a unit kill + fresh
   systemd-run if the unit was created transiently — the assertion is that the
   tmux window SURVIVES because the birth wrap put it outside the relay's
   cgroup), and assert the window and pane are still alive afterward.
   `make restart-survival-host` must exercise the same relay-lifecycle
   sequence on the host (relay as the unit, not tmux); document in
   docs/REMEDIATION-19.md exactly what it runs. The EXIT runs the test twice in
   a row green; if the environment genuinely cannot host a user manager the
   test may skip BOTH times, but then docs/REMEDIATION-19.md must show the host
   target's full output doing the relay-lifecycle proof instead, and the host
   target's run is a required part of the evidence. A vacuous skip with no
   substitute evidence fails this phase.
2. INTERACTION ACKS (m5): every Approve/Deny/picker interaction must be
   acknowledged within Discord's deadline — respond to the interaction callback
   (deferred update or response) so the user does not see "interaction failed";
   the ACK must precede keystroke handling. `TestInteraction_Ack`: click approve
   via the mock gateway, assert the ACK was sent (mock records it) AND the pinned
   keystrokes reached the pane.
3. ONE-SHOT FLAGS (m5): approvalPosted/questionPosted are per-prompt, not
   per-session — a second permission prompt in the same session gets its own
   buttons/menu. `TestApproval_SecondPromptGetsButtons` (fake CLI renders two
   sequential prompts; two button messages appear; the first is finalized
   before the second).
4. COMPONENT SENDS THROUGH THE THROTTLE (m5): SendComponents/EditComponents go
   through the same throttle + 429 backoff as every other Discord write.
5. TMUX RESOLUTION (m6): every production tmux invocation goes through the
   resolver (RELAY_TMUX_BIN → /usr/local/bin/tmux if usable → PATH), including
   watcher TTL live-window checks and inject.go.
6. TELEMETRY SHAPE (m6): POST body is {username: sha256(username)[:12] (12 hex
   chars), version} exactly; daily-once and quiet-failure are tested (unit).
7. ISOLATION LOG (m6): relay logs its isolation mode (systemd-run wrap vs
   fallback) at startup, not only at dpmux spawn.
8. DPMUX UPDATE (m6): `dpmux update` must work from an installed ~/.local/bin
   dpmux (not just inside a checkout): record the install source (e.g. a
   marker written by install.sh naming the checkout or the release tag) and
   pull+rebuild from it, or clone fresh when no source is known; prompt to
   restart the service (read from tty when present, else print the instruction).
   A unit test with a fake HOME covers the marker path.
Write docs/REMEDIATION-19.md mapping each finding to its fix and test evidence,
no PENDING entries. Commit exactly:
`fix: real restart survival, interaction acks, update path, telemetry shape`.
TIMEOUT: 3600
ATTEMPTS: 3
EXIT: test -f docs/ISOLATION-MATRIX.md
EXIT: grep -c '^RUN [0-9]*/5: PASS' docs/ISOLATION-MATRIX.md | grep -q '^5$'
EXIT: ! grep -qE '^RUN [0-9]*/5: FAIL' docs/ISOLATION-MATRIX.md
EXIT: grep -q 'uncached' docs/ISOLATION-MATRIX.md
EXIT: grep -qE 'no (pre-existing|existing) tmux server' docs/ISOLATION-MATRIX.md

The isolation matrix split out of phase 15 (DECISION phase15-split). Prove
TestInvariant4_ResumeRefusalRetry is repeatably green with no environmental
crutch. Run it FIVE times, each in a freshly built environment: rebuild the
integration image (`docker compose -f docker-compose.test.yml build --no-cache`)
before the runs or as needed to honor 'uncached', and pass `-count=1` so no Go
test cache is reused. Each run must start with no pre-existing tmux server in
its namespace; capture per run the exact command, exit code, and pass/fail of
the named test into docs/ISOLATION-MATRIX.md in the form `RUN <n>/5: PASS` (or
FAIL). Do not modify application code, tests, or docs/REMEDIATION-15.md in this
phase: this is verification only. If a run fails, record the failure verbatim
with its output and stop — a FAIL is evidence, not something to retry away; the
phase must not end green on a failing run. If you find a genuine environmental
flake, record it with the exact output and mark the phase failed honestly.
Final lines: the path written and the five RUN lines.

Pause before milestone 5 for the operator to inspect the new phase-15 cross reviews
and docs/REMEDIATION-15.md. This is a fresh sentinel, separate from the publication
gate; no worker writes it. An existing BLOCKING verdict or red/uncorroborated test run
requires remediation, not acceptance merely because the engine permits split reviews.

## Phase 20: integrity and update-path remediation (implementer)
AFTER: 19
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -q 'marker_found' test/integration/dpmux_test.go
EXIT: make integration T='TestDpmux_MarkerBeforeSpawn|TestDpmux_NewWindowMarkerBeforeSpawn' > /tmp/p20-m.log 2>&1; grep -q -- '--- PASS: TestDpmux_MarkerBeforeSpawn ' /tmp/p20-m.log && grep -q -- '--- PASS: TestDpmux_NewWindowMarkerBeforeSpawn ' /tmp/p20-m.log
EXIT: make integration T='TestInteraction_ApproveDrivesApproveRecipe|TestInteraction_DenyDrivesDenyRecipe' > /tmp/p20-i.log 2>&1; grep -q -- '--- PASS: TestInteraction_ApproveDrivesApproveRecipe ' /tmp/p20-i.log && grep -q -- '--- PASS: TestInteraction_DenyDrivesDenyRecipe ' /tmp/p20-i.log
EXIT: CGO_ENABLED=0 /usr/local/go/bin/go test -count=1 -v -run '^TestResolveTmuxBin$' ./internal/tmuxctl > /tmp/p20-r.log 2>&1; grep -q -- '--- PASS: TestResolveTmuxBin ' /tmp/p20-r.log
EXIT: CGO_ENABLED=0 /usr/local/go/bin/go test -count=1 -v -run '^TestDpmuxUpdate_ChecksOutLatestTag$' ./cmd/dpmux > /tmp/p20-u.log 2>&1; grep -q -- '--- PASS: TestDpmuxUpdate_ChecksOutLatestTag ' /tmp/p20-u.log
EXIT: ! grep -rnE '"tm" *\+ *"ux"' --include='*.go' cmd internal test
EXIT: ! grep -rnE 'exec\.Command(Context)?\([^)]*"tmux"' --include='*.go' cmd internal | grep -v '_test.go' | grep -v '^internal/tmuxctl/'
EXIT: for n in 1 2 3 4 5; do l=$(grep -- '--- PASS: TestInvariant4_ResumeRefusalRetry' docs/isolation-logs/run-$n.log) && grep -qF -- "$l" docs/ISOLATION-MATRIX.md || exit 1; done
EXIT: test -s docs/REMEDIATION-20.md && ! grep -q 'PENDING' docs/REMEDIATION-20.md
EXIT: git log -1 --format=%s | grep -qxF 'fix: restore marker ordering gates, honest evidence, latest-tag update'

STATE: HEAD 0475179; `make test` is green. Read first:
/root/.system/runs/run_20260904T054447_438b65/phase-19/review/reviewer-b/review.md and
/root/.system/runs/run_20260904T054447_438b65/phase-9/review/reviewer-a/review.md.
Fix exactly these findings; nothing else. Never weaken an assertion to go green.
1. Restore the ordering gates: the dpmux stub child records at its OWN startup whether
   the pending marker already existed (`marker_found`), and both
   TestDpmux_MarkerBeforeSpawn and TestDpmux_NewWindowMarkerBeforeSpawn assert it.
2. Evidence honesty: re-run the five uncached isolation runs (phase 18 method), save each
   full output to docs/isolation-logs/run-<n>.log, and rewrite the per-run lines in
   docs/ISOLATION-MATRIX.md by copying the PASS/FAIL lines verbatim from those logs. Correct
   docs/REMEDIATION-19.md so it cites only tests that exist and assert something.
3. Add a real TestResolveTmuxBin (RELAY_TMUX_BIN override, /usr/local/bin fallback when
   usable, PATH last; use temp dirs and stub binaries) and delete the no-op TestImport.
4. Replace TestInteraction_Ack with TestInteraction_ApproveDrivesApproveRecipe and
   TestInteraction_DenyDrivesDenyRecipe: the fake CLI prints distinct pane text for allow
   vs reject, each test asserts the ACK was recorded and its own text appeared and the other
   did not.
5. Remove the "tm"+"ux" split; tmuxctl owns the literal "tmux" as its PATH fallback.
6. Revert the out-of-scope ORDER BY/owner-query changes in test/integration/spawn_test.go
   unless a test genuinely needs them; if kept, justify with evidence in REMEDIATION-20.md.
7. `dpmux update`: fetch tags, check out the `latest` tag (detached is fine), rebuild and
   reinstall the binaries, then prompt to restart the service (tty prompt, else print the
   command). TestDpmuxUpdate_ChecksOutLatestTag uses a temp bare repo with a `latest` tag
   pointing at a commit different from the branch tip and asserts that commit is checked out.
Write docs/REMEDIATION-20.md mapping each finding to fix + evidence. Stage only files you
changed plus docs/isolation-logs/. Commit exactly:
`fix: restore marker ordering gates, honest evidence, latest-tag update`.

## Phase 21: marker visibility under scope wrap, flaky suite, honest restart proof (implementer)
AFTER: 22
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: d=$(mktemp -d); make test > $d/t1.log 2>&1 || { tail -40 $d/t1.log; exit 1; }
EXIT: d=$(mktemp -d); make test > $d/t2.log 2>&1 || { tail -40 $d/t2.log; exit 1; }
EXIT: d=$(mktemp -d); make test > $d/t3.log 2>&1 || { tail -40 $d/t3.log; exit 1; }
EXIT: grep -rq 'func TestDpmux_MarkerBeforeSpawn_SystemdScope' test/integration
EXIT: for i in 1 2 3; do make integration T='TestDpmux_MarkerBeforeSpawn|TestDpmux_MarkerBeforeSpawn_SystemdScope|TestDpmux_NewWindowMarkerBeforeSpawn' > /tmp/p21-m$i.log 2>&1; for t in TestDpmux_MarkerBeforeSpawn TestDpmux_MarkerBeforeSpawn_SystemdScope TestDpmux_NewWindowMarkerBeforeSpawn; do grep -q -- "--- PASS: $t " /tmp/p21-m$i.log || exit 1; done; done
EXIT: make integration T=TestInject_ReadinessProbe > /tmp/p21-r.log 2>&1; grep -q -- '--- PASS: TestInject_ReadinessProbe ' /tmp/p21-r.log
EXIT: make integration T=TestRestartSurvival_DisposableUnit > /tmp/p21-s.log 2>&1; grep -q -- '--- PASS: TestRestartSurvival_DisposableUnit ' /tmp/p21-s.log && ! grep -q -- '--- SKIP' /tmp/p21-s.log
EXIT: test -s docs/REMEDIATION-21.md && ! grep -q 'PENDING' docs/REMEDIATION-21.md
EXIT: git log -1 --format=%s | grep -qxF 'fix: marker visible under scope wrap, deterministic readiness, strict restart proof'

STATE (re-run): commit 5e40049 landed this phase's marker and restart fixes; phase 22 then
fixed the readiness flake. This re-run only verifies: do not redo committed work; if HEAD
already carries both commits, re-land nothing and let the EXITs judge. Original brief:
STATE: HEAD 5fbfd06. Read first:
/root/.system/runs/run_20260904T054447_438b65/phase-9/review/reviewer-a/review.md,
/root/.system/runs/run_20260904T054447_438b65/phase-9/review/reviewer-b/review.md and
/root/.system/runs/run_20260904T054447_438b65/phase-20/review/reviewer-a/review.md.
The suite is intermittent: the engine saw it pass, three independent reviewer runs saw it
fail. Fix causes, never loosen an assertion or raise a timeout to hide a race.
1. Marker ordering under the scope wrap: dpmux must make the pending marker durable
   (write + fsync + rename into place) before any tmux birth, and the systemd-run
   `--scope` birth path (internal/tmuxctl NewBirthCommand) must give the child the same
   RELAY_STATE_DIR/HOME/environment dpmux used, so the child sees the marker it was paired
   with. Add TestDpmux_MarkerBeforeSpawn_SystemdScope, which forces the scope wrap and
   asserts the child's `marker_found=true`. It may skip only when systemd-run --user is
   unavailable, logging the probe failure; in this container the probe succeeds, so the
   EXIT requires PASS three times.
2. TestInject_ReadinessProbe: find why the prompt sometimes never reaches the DB (a probe
   race against FAKE_DP_SWALLOW_UNTIL_READY, a send before the ready signature, or leaked
   state from an earlier test) and fix the cause.
3. TestRestartSurvival_DisposableUnit: if `systemctl --user restart` of the original relay
   unit fails, the test FAILS. No stop-and-replace substitute.
4. Run the full suite repeatedly before committing. Record in docs/REMEDIATION-21.md the
    root cause of each flake, the fix, and three consecutive green `make test` runs.
Commit exactly: `fix: marker visible under scope wrap, deterministic readiness, strict restart proof`.

## Phase 22: gateway readiness and flake-free suite (implementer)
AFTER: 20
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: grep -q 'FAKE_DP_SWALLOW_UNTIL_READY=1' test/integration/inject_test.go && grep -q 'FAKE_DP_READY_DELAY_MS=3000' test/integration/inject_test.go
EXIT: grep -rq 'func TestGateway_DispatchBeforeReadyIsDelivered' test/integration
EXIT: d=$(mktemp -d); make integration T=TestGateway_DispatchBeforeReadyIsDelivered > $d/g.log 2>&1; grep -q -- '--- PASS: TestGateway_DispatchBeforeReadyIsDelivered ' $d/g.log || { tail -40 $d/g.log; exit 1; }
EXIT: d=$(mktemp -d); for i in $(seq 10); do make integration T=TestInject_ReadinessProbe > $d/r$i.log 2>&1; grep -q -- '--- PASS: TestInject_ReadinessProbe ' $d/r$i.log || { echo "run $i"; tail -40 $d/r$i.log; exit 1; }; done
EXIT: d=$(mktemp -d); for i in 1 2 3 4 5; do make test > $d/t$i.log 2>&1 || { echo "run $i"; tail -60 $d/t$i.log; exit 1; }; done
EXIT: test -s docs/REMEDIATION-22.md && ! grep -q 'PENDING' docs/REMEDIATION-22.md
EXIT: git diff --quiet HEAD -- cmd internal test docs && test -z "$(git ls-files --others --exclude-standard cmd internal test docs)"
EXIT: git log -1 --format=%s | grep -qxF 'fix: gateway readiness gate, flake-free readiness probe'

RECOVERY (read first): HEAD a03d977 carries this subject but both reviewers BLOCKED it.
Read /root/.system/runs/run_20260904T054447_438b65/phase-22/review/reviewer-a/review.md
and .../reviewer-b/review.md. The working tree already holds uncommitted fixes for their
findings: cmd/mock-discord/main.go (release held dispatches only once the session is
consuming, e.g. after its first heartbeat), cmd/relay/main.go (RESUMED handling),
test/integration/dpmux_test.go (stub ignores the `-V`/`--version` probe dpmux runs before
writing the marker), test/integration/inject_test.go (MOCK_READY_DELAY_MS precondition
that forces dispatch before READY), plus untracked logs under docs/flake-logs/. Start from
that tree; do not discard it. Then: (a) verify TestGateway_DispatchBeforeReadyIsDelivered
FAILS with the hold disabled and PASSES with it, keeping both logs; (b) confirm the
version-probe race was the marker flake's root cause and state it in
docs/REMEDIATION-22.md, removing the "undiagnosed" residual wording unless it recurs;
(c) fix every log citation so the quoted line number matches the file exactly; (d) run the
EXIT loops yourself, save logs under docs/flake-logs/ and commit them. Commit only cmd/,
internal/, test/ and docs/ changes. Never stage plans/, .tmp/ or any gate sentinel. Make a
NEW commit with the exact subject below; do not amend.
STATE: HEAD 5e40049. Read first:
/root/.system/runs/run_20260904T054447_438b65/phase-21/review/reviewer-a/review.md and
/root/.system/runs/run_20260904T054447_438b65/phase-21/review/reviewer-b/review.md.
Reviewer-b's diagnosis is the starting hypothesis: a /control/dispatch can be queued while
the relay's gateway is still in HELLO/IDENTIFY/READY (or after a failed Open()) and is
never consumed, so the prompt never reaches the pane. Confirm it with a reproduction before
fixing. Never loosen an assertion, raise a wait, or change the fake's readiness knobs.
1. Relay: expose gateway readiness (log line plus a state the tests can observe, e.g. a
   ready file under RELAY_STATE_DIR written on the READY event). A failed or dropped
   gateway Open() must retry or exit non-zero, never keep running deaf to inbound events.
2. Mock Discord: /control/dispatch holds events until a connected session has sent
   IDENTIFY and received READY (bounded wait, clear error if none), then delivers them.
3. Tests: every test that dispatches gateway events waits for relay readiness first. Put
   the wait in one shared helper.
4. Add TestGateway_DispatchBeforeReadyIsDelivered: dispatch immediately after starting the
   relay, before READY, and assert the event is still handled exactly once.
5. docs/REMEDIATION-22.md: root cause with the reproduction evidence, the fix, and results
   of the repeated runs. Keep full logs under docs/flake-logs/ and quote from them
   verbatim; never cite a shared /tmp path.
Commit exactly: `fix: gateway readiness gate, flake-free readiness probe`.

## Phase 23: pre-publish deployment and security fixes (implementer)
AFTER: 10
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: d=$(mktemp -d); make test > $d/t.log 2>&1 || { tail -60 $d/t.log; exit 1; }
EXIT: d=$(mktemp -d); make integration T='TestRelay_RefusesWithoutUserID|TestInstalledUnit_ResolvesDpmux' > $d/i.log 2>&1; grep -q -- '--- PASS: TestRelay_RefusesWithoutUserID ' $d/i.log && grep -q -- '--- PASS: TestInstalledUnit_ResolvesDpmux ' $d/i.log || { tail -60 $d/i.log; exit 1; }
EXIT: d=$(mktemp -d); CGO_ENABLED=0 /usr/local/go/bin/go test -count=1 -v -run '^TestShouldAcceptAuthor_EmptyIDRejectsAll$' ./internal/inject > $d/a.log 2>&1; grep -q -- '--- PASS: TestShouldAcceptAuthor_EmptyIDRejectsAll ' $d/a.log || { tail -40 $d/a.log; exit 1; }
EXIT: d=$(mktemp -d); CGO_ENABLED=0 /usr/local/go/bin/go test -count=1 -v -run '^(TestDpmuxUpdate_ReplacesRunningBinary|TestDpmuxUpdate_FollowsMovedLatestTag|TestDpmuxUpdate_ChecksOutLatestTag)$' ./cmd/dpmux > $d/u.log 2>&1; grep -q -- '--- PASS: TestDpmuxUpdate_ReplacesRunningBinary ' $d/u.log && grep -q -- '--- PASS: TestDpmuxUpdate_FollowsMovedLatestTag ' $d/u.log && grep -q -- '--- PASS: TestDpmuxUpdate_ChecksOutLatestTag ' $d/u.log || { tail -40 $d/u.log; exit 1; }
EXIT: d=$(mktemp -d); CGO_ENABLED=0 /usr/local/go/bin/go test -count=1 -v -run '^TestTelemetry_SendsAgainAfterDayBoundary$' ./internal/telemetry > $d/m.log 2>&1; grep -q -- '--- PASS: TestTelemetry_SendsAgainAfterDayBoundary ' $d/m.log || { tail -40 $d/m.log; exit 1; }
EXIT: grep -qE 'RELAY_DPMUX_BIN=%h/\.local/bin/dpmux|PATH=%h/\.local/bin' systemd/devpass-discord-relay.service
EXIT: ! grep -qi 'non-root' README.md && grep -q 'TELEMETRY_URL' README.md && grep -q 'DISCORD_USER_ID' README.md
EXIT: grep -q '\.config/devpass-discord-relay/\.env' SETUP.md
EXIT: test -s docs/REMEDIATION-23.md && ! grep -q 'PENDING' docs/REMEDIATION-23.md
EXIT: git diff --quiet HEAD -- cmd internal test docs systemd install.sh README.md SETUP.md CONTRIBUTING.md CHANGELOG.md
EXIT: git log -1 --format=%s | grep -qxF 'fix: fail-closed user gate, installed-service PATH, atomic update, daily telemetry, honest docs'

STATE: HEAD 1014c16. Read first:
/root/.system/runs/run_20260904T054447_438b65/phase-9/review/reviewer-a/review.md,
/root/.system/runs/run_20260904T054447_438b65/phase-10/review/reviewer-a/review.md and
/root/.system/runs/run_20260904T054447_438b65/phase-10/review/reviewer-b/review.md.
Fix exactly these findings. Never loosen an assertion, skip a test, or raise a timeout.
1. Fail-closed user gate: the relay refuses to start (non-zero exit, one clear log line
   naming DISCORD_USER_ID) when DISCORD_USER_ID is unset or empty, and
   ShouldAcceptAuthor rejects every author when the configured ID is empty.
   TestRelay_RefusesWithoutUserID (test/integration) starts relay without the variable and
   asserts exit status and message; TestShouldAcceptAuthor_EmptyIDRejectsAll
   (internal/inject) is a unit test.
2. Installed-service PATH: systemd/devpass-discord-relay.service sets
   Environment=RELAY_DPMUX_BIN=%h/.local/bin/dpmux and puts %h/.local/bin first on PATH.
   TestInstalledUnit_ResolvesDpmux (test/integration) installs into a temp HOME via
   install.sh, starts the relay with the unit's Environment lines and a minimal PATH
   (/usr/bin:/bin) under a disposable systemd-run --user unit, triggers a spawn, and asserts
   dpmux ran. It may skip only when systemd-run --user is unavailable, logging the probe
   failure; the EXIT requires PASS.
3. dpmux update writes each new binary to a temp file in the same directory, chmods it,
   and renames it over the old one. TestDpmuxUpdate_ReplacesRunningBinary keeps a copy of
   the old binary running during the update and asserts the update succeeds.
4. dpmux update runs git fetch --tags --force so a moved latest tag replaces the local one.
   TestDpmuxUpdate_FollowsMovedLatestTag moves the tag in the temp remote and asserts the
   new commit is checked out. Keep TestDpmuxUpdate_ChecksOutLatestTag passing.
5. Telemetry: when TELEMETRY_URL is set, a long-running relay sends once per UTC day, not
   only at startup (injectable clock). TestTelemetry_SendsAgainAfterDayBoundary crosses a
   day boundary without restarting and asserts a second send; still at most one per day.
6. Docs: README security section drops the non-root claim (it runs under your own
   account), states the relay refuses to start without DISCORD_USER_ID, and names
   telemetry (off unless TELEMETRY_URL is set; daily; sha256(username)[:12] and version
   only). SETUP.md names ~/.config/devpass-discord-relay/.env as the file the service
   reads and demotes the repo-root .env to a dev-only fallback. CONTRIBUTING.md states
   FAKE_DP_REFUSE_FIRST_RESUME=1 refuses every --session launch while set. CHANGELOG.md
   telemetry wording matches the code.
7. docs/REMEDIATION-23.md maps each finding to fix and test evidence. Save the EXIT runs'
   logs under docs/flake-logs/ and quote them verbatim.
   Stage only cmd/, internal/, test/, docs/, systemd/, install.sh and the four docs. Never
   stage plans/, .tmp/ or any gate sentinel. New commit, do not amend. Commit exactly:
   `fix: fail-closed user gate, installed-service PATH, atomic update, daily telemetry, honest docs`.
