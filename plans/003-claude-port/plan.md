# LLMDiscordRelay: port DevPassDiscordRelay from devpass-code to Claude Code, release v1.0.0
WORKDIR: /root/devpass-discord-relay

DECISION wrap-up: operator chose "release v1.0.0": port, unit + Docker integration green, docs rewritten for Claude, DevPass-era docs archived, CHANGELOG 1.0.0, tag v1.0.0, rename the GitHub repo to LLMDiscordRelay, switch the running service to the new relay.
DECISION launch-cmd: the agent command is configurable via RELAY_AGENT_CMD, default `claude` (operator may set it to `pl`).
DECISION cli-name: the launcher is `llmux`. Env vars move from DEVPASS_*/RELAY_DEVPASS_* to LLM_RELAY_*/RELAY_AGENT_*; config dir ~/.config/llm-discord-relay, state dir ~/.llm-discord-relay; an existing ~/.config/devpass-discord-relay/.env is migrated automatically (copied once, never deleted).

Branch `claude-port` already holds: module path github.com/syedmusaasad/LLMDiscordRelay, cmd/dpmux moved to cmd/llmux (code inside still says dpmux), systemd unit renamed to systemd/llm-discord-relay.service (contents unchanged), a NEW JSONL transcript reader in internal/transcript/transcript.go (replaces the SQLite reader; not yet compiled or tested), and a real sanitized transcript fixture at testdata/claude/-work-demo/11111111-2222-4333-8444-555555555555.jsonl. The tree does not compile yet. Work on branch claude-port only; never push, never touch main, until phase 6.

Verified Claude Code 2.1.319 behaviour (probed in tmux on this host; build on these facts, do not re-derive them):
- Identity: launch with `claude --session-id <uuid4>`. The id is chosen by the launcher, so the old stderr-log binding protocol (--print-logs, message=created) is obsolete; the pending marker carries the session id directly. Resume: `claude --resume <uuid>`. A bad id prints `No conversation found with session ID: <id>` and the process exits 1.
- Transcript: $CLAUDE_CONFIG_DIR/projects (default ~/.claude/projects)/<project-slug>/<session-id>.jsonl, created only when the first prompt is submitted (not at launch). Line types and their meaning are implemented in internal/transcript ParseLine: user prompt = type "user" with string content (skip isMeta, isSidechain, and text starting with <command-, <local-command-, <system-reminder>); assistant text/tool_use blocks grouped by message.id; tool results are type "user" with tool_result blocks (is_error); a turn ends at type "system" subtype "turn_duration", or at a user line starting "[Request interrupted by user" (interrupts get no turn_duration). There is no SQLite DB.
- Trust dialog: in a directory not yet trusted, the TUI first shows "Quick safety check: Is this a project you created or one you trust?" with "❯ No, exit" selected and "Yes, I trust this folder" below. Keys: Down, Enter accepts. Relay must surface this to Discord as an approval (Approve = trust, Deny/timeout = Esc), never auto-accept.
- Ready/idle pane: an input line starting with "❯ " between two "────" rules, and the footer line does NOT contain "esc to interrupt". Busy: footer contains "esc to interrupt". The old "Ask anything..." / "Pl-Interactive" signatures are gone.
- Permission prompt (permission-mode default): pane contains "Do you want to proceed?", numbered options "❯ 1. Yes", "2. Yes, and always allow ...", ..., last option "N. No", and "Esc to cancel". Approve = key "1". Deny = Escape (pressing the "No" digit also interrupts the turn). Signature for de-duplication = the command/action text block shown between the ╌╌╌ rules.
- AskUserQuestion picker: pane shows "☐ <header>", the question line, options "❯ 1. Red" / "2. Green" with description lines indented beneath, then "N. Type something.", "Chat about this", and footer "Enter to select · ↑/↓ to navigate · Esc to cancel". Post only the real options (exclude "Type something." and "Chat about this"). Select option i = Down x i then Enter.
- Multi-line input: `tmux send-keys -l` submits at the first newline. Use `tmux load-buffer -b <name> <file>` + `tmux paste-buffer -p -d -b <name> -t <target>` (bracketed paste) then send Enter; verified a 3-line prompt arrives as one prompt.
- Process exit: when claude exits, the pane's command ends; llmux must keep the existing liveness semantics.

## Phase 1: port core to Claude Code (implementer)
TIMEOUT: 7200
ATTEMPTS: 2
REVIEW: on
EXIT: export PATH=/usr/local/go/bin:$PATH CGO_ENABLED=0 && go build ./... && go vet ./... && make unit
EXIT: ! grep -rInE 'devpass|opencode|ses_[A-Za-z0-9]|Ask anything|Pl-Interactive|message=created|dpmux' --include=*.go cmd internal
EXIT: test -x bin/llmux || (export PATH=/usr/local/go/bin:$PATH && make build && test -x bin/llmux)
EXIT: git log -1 --format=%s | grep -qxF 'feat: run Claude Code sessions instead of devpass-code'

Make the relay drive Claude Code. Scope: cmd/relay, cmd/llmux, internal/** and their unit tests, Makefile, go.mod/go.sum. Integration tests (test/integration), Dockerfile.test and docs are later phases: do not edit them.
1. internal/transcript: finish and unit-test the new reader against testdata/claude (ParseLine kinds, sidechain/meta/harness skipping, interrupt as turn end, partial-line tailing, Store.Path refusing non-UUID ids, List/Info). Delete the SQLite code and drop modernc.org/sqlite from go.mod if nothing else needs it (`go mod tidy`).
2. internal/mirror: replace every *sql.DB use. scanSessions pairs pending markers (which now always carry sessionID) with transcripts that exist; mirrorContent consumes transcript.Event (one Discord message per assistant message.id, streamed by edits as more blocks arrive; tool embeds keyed by tool_use id, marked done/failed by KindToolResult; `_— N tool(s)_` summary and "Turn finished without output" at KindTurnEnd). Delete the stderr-log binder, createdSessionLogRe, extractCreatedSessionID and their callers. handleSpawnFromDiscord: llmux generates the uuid, relay waits for readiness (handling the trust dialog via Discord approval), sends the first prompt, then waits for the transcript to appear (bind timeout) before mirroring; same fail-closed teardown on failure. Resume refusal = pane shows "No conversation found with session ID" or the pane dies before ready. waitForEcho = transcript.Store.HasPrompt. !resume candidate list comes from Store.List (show title, model, age instead of provider).
3. internal/inject + internal/tmuxctl: new PaneReady per the facts above; multi-line send via load-buffer/paste-buffer; recipes: ApprovalApprove {"1"}, ApprovalDeny {"Escape"}, TrustAccept {"Down","Enter"}, TrustDeny {"Escape"}, question select Down x i + Enter. Parsers for permission prompt, trust dialog and AskUserQuestion with unit tests built from the pane texts quoted in the preamble.
4. cmd/llmux (was dpmux): binary name, usage text, session name prefix `llm-`, launch `$RELAY_AGENT_CMD` (default `claude`; split on whitespace, first word resolved on PATH) with `--session-id <uuid>` for new sessions or `--resume <uuid>` for resumes (`llmux --resume <uuid>`), no --auto/--print-logs/stderr redirect. Keep `ls`, `update`, `uninstall`, tmux socket/scope logic and marker atomicity. Env: RELAY_AGENT_CMD, RELAY_STATE_DIR (default ~/.llm-discord-relay), RELAY_CLAUDE_PROJECTS.
5. cmd/relay: env file lookup $LLM_RELAY_ENV, then ~/.config/llm-discord-relay/.env, then ./.env; if the new file is missing and ~/.config/devpass-discord-relay/.env exists, copy it (mode 0600) and log that it did. Replace RELAY_DEVPASS_DB / DBPath with the projects root. Config field DpmuxBin -> LlmuxBin (env RELAY_LLMUX_BIN).
6. Replace cmd/fake-devpass-code with cmd/fake-claude: a test double that accepts `--session-id`/`--resume`, prints the trust dialog when FAKE_CLAUDE_UNTRUSTED=1, renders the ready pane and the permission/question panes verbatim from the preamble, refuses unknown --resume ids with the real message and exit 1, and appends real-shaped JSONL lines (copy shapes from the fixture) to $RELAY_CLAUDE_PROJECTS/<slug>/<id>.jsonl, creating the file on the first prompt. Script turns via FAKE_CLAUDE_SCRIPT (same JSON array format as before). Build it in the Makefile in place of fake-devpass-code.
Run `make unit` once at the end and fix what fails. git add only the paths in scope (plus deleted files). Commit subject exactly: `feat: run Claude Code sessions instead of devpass-code`.

## Phase 2: integration suite on fake-claude (implementer)
TIMEOUT: 7200
ATTEMPTS: 2
EXIT: export PATH=/usr/local/go/bin:$PATH && make integration
EXIT: ! grep -rInE 'devpass|opencode|FAKE_DP_|dpmux|ses_[A-Za-z0-9]' test Dockerfile.test docker-compose.test.yml Makefile
EXIT: git log -1 --format=%s | grep -qxF 'test: port integration suite to fake-claude'

Port test/integration (spawn, mirror, inject, resume, smoke, restart survival, deploy, llmux, discord429) plus Dockerfile.test and docker-compose.test.yml from fake-devpass-code/opencode.db to cmd/fake-claude and JSONL transcripts. Keep every behaviour each test proves (fail-closed spawn, one-thread-per-session, resume refusal then fresh session, duplicate-echo suppression, approval and question round-trips, restart survival, 429 handling); replace DB seeding with writing transcript files. Add one test for the trust-dialog approval round-trip and one for a 3-line Discord message arriving as one prompt. Fix product code only if a test exposes a real bug, and say so in the commit body. Run `make integration` (Docker) once at the end. Commit subject exactly: `test: port integration suite to fake-claude`.

## Phase 3: live check against real Claude Code (implementer)
TIMEOUT: 2400
ATTEMPTS: 2
EXIT: bash test/live/claude_smoke.sh
EXIT: git log -1 --format=%s | grep -qxF 'test: live smoke against real Claude Code'

Write test/live/claude_smoke.sh (not run in CI; requires a signed-in `claude` on PATH and tmux): build bin/, start cmd/mock-discord and the relay on an isolated tmux socket and temp RELAY_STATE_DIR, with RELAY_AGENT_CMD="claude --model haiku --effort low --permission-mode default" and a temp project dir it pre-trusts by accepting the trust dialog through the relay's approval path. Assert, via mock-discord's recorded calls: a channel message spawns a session and a thread; the prompt echo and the assistant reply are mirrored; a 2-line reply injected into the thread arrives as one prompt; a prompt that triggers a Bash permission produces an approval message and Approve lets it run; killing the pane then replying resumes the same session id (`--resume`); `llmux --resume` with an unknown uuid is detected as a refusal. Keep prompts tiny (haiku, low effort). Exit 0 only if every assertion passes; print each check. Fix product bugs it exposes in the same commit and list them in the body. Commit subject exactly: `test: live smoke against real Claude Code`.

## Phase 4: docs for LLMDiscordRelay (document-writer)
TIMEOUT: 1800
ATTEMPTS: 2
EXIT: ! grep -nIiE 'devpass|opencode|dpmux' README.md SETUP.md CONTRIBUTING.md docs/TRANSCRIPT-SCHEMA.md docs/BINDING-PROTOCOL.md install.sh systemd/llm-discord-relay.service
EXIT: test -d docs/archive/devpass && grep -q '^## \[1.0.0\]' CHANGELOG.md
EXIT: git log -1 --format=%s | grep -qxF 'docs: LLMDiscordRelay for Claude Code'

Rewrite README.md, SETUP.md, CONTRIBUTING.md for LLMDiscordRelay on Claude Code: keep the architecture diagram (claude in tmux; transcript JSONL read-only instead of opencode.db), the pane-vs-transcript split, the private-guild privacy section and the security posture (update paths: ~/.config/llm-discord-relay/.env, state ~/.llm-discord-relay, LLM_RELAY_ENV), usage with `llmux`, `llmux ls`, `llmux --resume <id>`, RELAY_AGENT_CMD (default claude; e.g. `pl`), the trust-dialog approval, and migration notes from DevPassDiscordRelay (old .env copied automatically; old service must be disabled). Rewrite docs/TRANSCRIPT-SCHEMA.md and docs/BINDING-PROTOCOL.md from the preamble facts and internal/transcript. Move docs/REMEDIATION-*.md, DISCOVERY.md, ISOLATION-MATRIX.md, isolation-logs/, TOOLCHAIN.md, NOTES.md and plans/000-discovery, plans/001-build, plans/002-live-fixes into docs/archive/devpass/ with `git mv` and a short docs/archive/devpass/README.md saying they describe the retired devpass-code relay. Update install.sh (installs relay + llmux, copies systemd/llm-discord-relay.service, uses the new config dir) and the unit file's Description/ExecStart/EnvironmentFile. Add `## [1.0.0] - 2026-11-08` at the top of CHANGELOG.md listing the rename, Claude Code support, removed DevPass support, and the env/path migration. Do not edit Go code. Commit subject exactly: `docs: LLMDiscordRelay for Claude Code`.

## Phase 5: approve release (gate)
GATE: plans/003-claude-port/.approve-release

## Phase 6: release and switch over (implementer)
LANDING: on
TIMEOUT: 1800
ATTEMPTS: 1
EXIT: git rev-parse -q --verify refs/tags/v1.0.0 >/dev/null && test "$(git rev-parse main)" = "$(git rev-parse origin/main)"
EXIT: git ls-remote --tags https://github.com/syedmusaasad/LLMDiscordRelay.git v1.0.0 | grep -q v1.0.0
EXIT: systemctl --user is-active llm-discord-relay.service && ! systemctl --user is-enabled devpass-discord-relay.service

The operator approved the release. In order, stopping at the first failure:
1. Rename the GitHub repo: `curl -fsS -X PATCH -H "Authorization: Bearer $(cat /root/.config/pipeline/github-token)" -H "Accept: application/vnd.github+json" https://api.github.com/repos/syedmusaasad/DevPassDiscordRelay -d '{"name":"LLMDiscordRelay","description":"Mirror Claude Code tmux sessions to private Discord threads and reply from Discord."}'` (skip if https://api.github.com/repos/syedmusaasad/LLMDiscordRelay already returns 200). Then `git remote set-url origin git@github.com:syedmusaasad/LLMDiscordRelay.git`.
2. `git checkout main && git merge --no-ff claude-port -m 'release: LLMDiscordRelay v1.0.0'`, `git tag -a v1.0.0 -m 'LLMDiscordRelay v1.0.0'`, move the `latest` tag to it (`git tag -fa latest -m latest v1.0.0^{}`), `git push origin main` and `git push origin v1.0.0` and `git push -f origin latest`. Never force-push main.
3. Switch the service: `./install.sh` (new binaries, unit, config dir; confirm ~/.config/llm-discord-relay/.env exists and has DISCORD_BOT_TOKEN, DISCORD_CHANNEL_ID, DISCORD_USER_ID set — never print their values), then `systemctl --user disable --now devpass-discord-relay.service`, `systemctl --user daemon-reload`, `systemctl --user enable --now llm-discord-relay.service`. Wait 10 s and confirm `journalctl --user -u llm-discord-relay -n 30` shows the gateway READY and no error. Leave the old unit file and ~/.config/devpass-discord-relay in place (do not delete them). Remove ~/.local/bin/dpmux only if ~/.local/bin/llmux now exists.
Report the merge commit hash, the tag, and the service status in NOTES-RELEASE.md (untracked; do not commit it).
