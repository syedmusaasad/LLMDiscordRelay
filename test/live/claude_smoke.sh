#!/usr/bin/env bash
# Live smoke test: the relay against a REAL Claude Code, with a mock Discord.
#
# Not run in CI. Needs a signed-in `claude` on PATH, tmux, go, python3 and make.
# Everything runs on an isolated tmux socket (TMUX_TMPDIR) with a temp
# RELAY_STATE_DIR and a throwaway project directory; the host's tmux sessions and
# relay state are never touched. Claude Code keeps using the host's sign-in and
# config directory (transcripts for the throwaway project are removed at the end).
#
# The agent runs on haiku at low effort with permission-mode default, and every
# prompt is tiny. Assertions read what mock-discord recorded (REST calls) plus
# the Claude Code transcript and the tmux server.
#
# Exit 0 only if every check passes.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export PATH="/usr/local/go/bin:$HOME/.local/bin:$PATH"

pass=0
fail=0
failed=()
check() { # check "description" <command...>
  local desc="$1"
  shift
  if "$@"; then
    echo "PASS  $desc"
    pass=$((pass + 1))
  else
    echo "FAIL  $desc"
    fail=$((fail + 1))
    failed+=("$desc")
  fi
}
note() { echo "      $*"; }

for tool in claude tmux go python3 make; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "FAIL  prerequisite: $tool not on PATH"
    exit 1
  }
done

# ── workspace ────────────────────────────────────────────────────────────────
WORK="$(mktemp -d /tmp/llmlive-XXXXXX)"
PROJ="$WORK/project"
STATE="$WORK/state"
STATE2="$WORK/state-refusal"
mkdir -p "$PROJ" "$STATE" "$STATE2" "$WORK/tmux"
: >"$WORK/empty.env"

export TMUX_TMPDIR="$WORK/tmux"
unset TMUX

free_port() { python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
MOCK_PORT="$(free_port)"
GW_PORT="$(free_port)"
CHANNEL="live-channel"
CTL() { python3 -I "$ROOT/test/live/mockctl.py" "$MOCK_PORT" "$@"; }

MOCK_PID=""
RELAY_PID=""
SIDS=()
cleanup() {
  local rc=$?
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null
  # Explicit socket: never the host's tmux server, whatever the environment says.
  tmux -S "$TMUX_TMPDIR/tmux-$(id -u)/default" kill-server 2>/dev/null
  # Remove the transcripts this run created (their project dir is unique to $PROJ).
  local root="${RELAY_CLAUDE_PROJECTS:-${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects}"
  local f
  for sid in "${SIDS[@]}"; do
    for f in "$root"/*/"$sid".jsonl; do
      [ -e "$f" ] && rm -f "$f" && rmdir "$(dirname "$f")" 2>/dev/null
    done
  done
  if [ "$fail" -gt 0 ] || [ "$rc" -ne 0 ]; then
    echo "---- relay log (tail) ----"
    tail -n 60 "$WORK/relay.log" 2>/dev/null
    echo "---- state dir kept at $WORK for inspection ----"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

# ── helpers ──────────────────────────────────────────────────────────────────
wait_for() { # wait_for SECONDS command...   (polls every 0.5s)
  local deadline=$((SECONDS + $1))
  shift
  while [ "$SECONDS" -lt "$deadline" ]; do
    "$@" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

projects_root() { echo "${RELAY_CLAUDE_PROJECTS:-${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects}"; }

# transcript_files: every transcript of the throwaway project.
transcript_files() {
  local d
  for d in "$(projects_root)"/*"$(basename "$WORK")"*; do
    [ -d "$d" ] && ls "$d"/*.jsonl 2>/dev/null
  done
}

# prompts_of FILE: the user prompts (plain-string user lines) of a transcript.
prompts_of() {
  python3 -I -c '
import json, sys
for line in open(sys.argv[1]):
    try:
        e = json.loads(line)
    except ValueError:
        continue
    m = e.get("message") or {}
    if e.get("type") == "user" and isinstance(m.get("content"), str) and not e.get("isMeta"):
        print(json.dumps(m["content"]))
' "$1"
}

# count_ge CHANNEL SUBSTR N: at least N messages in CHANNEL contain SUBSTR.
count_ge() { [ "$(CTL count "$1" "$2")" -ge "$3" ]; }

# turn_ends CHANNEL: how many turn-complete markers the relay posted.
turn_ends() { CTL count "$1" "_— "; }

wait_turns() { # wait_turns CHANNEL N SECONDS
  local deadline=$((SECONDS + $3))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(turn_ends "$1")" -ge "$2" ] && return 0
    sleep 1
  done
  return 1
}

# non_echo_has CHANNEL TEXT: a message that is not the "You:" echo contains TEXT.
non_echo_has() {
  CTL texts "$1" | python3 -I -c '
import json, sys
needle = sys.argv[1]
for line in sys.stdin:
    t = json.loads(line)
    if needle in t and not t.startswith("**You:**"):
        sys.exit(0)
sys.exit(1)' "$2"
}

# click_approve CHANNEL COMPONENT_SUBSTR: click Approve on the first matching message.
click_approve() {
  local id
  id="$(CTL component "$2" 0)" || return 1
  CTL click "$1" "$id"
}

# ── build ────────────────────────────────────────────────────────────────────
echo "== build =="
(cd "$ROOT" && make build >"$WORK/build.log" 2>&1) || {
  cat "$WORK/build.log"
  echo "FAIL  make build"
  exit 1
}
BIN="$ROOT/bin"
note "claude: $(claude --version 2>&1 | head -1)"

# ── start mock-discord and the relay ─────────────────────────────────────────
echo "== start =="
MOCK_DISCORD_PORT="$MOCK_PORT" MOCK_DISCORD_GATEWAY_PORT="$GW_PORT" "$BIN/mock-discord" >"$WORK/mock.log" 2>&1 &
MOCK_PID=$!
wait_for 10 curl -sf "http://127.0.0.1:$MOCK_PORT/api/v10/gateway" || {
  echo "FAIL  mock-discord did not start"
  exit 1
}

# The relay runs inside the throwaway project directory: llmux starts the agent
# in its working directory, so that is the folder Claude Code asks to trust.
(
  cd "$PROJ" || exit 1
  exec env \
    DISCORD_TOKEN=live-token \
    DISCORD_CHANNEL_ID="$CHANNEL" \
    DISCORD_USER_ID=live-user \
    DISCORD_API_URL="http://127.0.0.1:$MOCK_PORT" \
    RELAY_STATE_DIR="$STATE" \
    RELAY_LLMUX_BIN="$BIN/llmux" \
    RELAY_AGENT_CMD="claude --model haiku --effort low --permission-mode default" \
    LLM_RELAY_ENV="$WORK/empty.env" \
    TELEMETRY_URL= \
    "$BIN/relay"
) >"$WORK/relay.log" 2>&1 &
RELAY_PID=$!
check "relay reaches gateway-ready" wait_for 20 test -e "$STATE/gateway-ready"

# ── 1. channel message spawns a session; trust dialog goes through approval ──
echo "== spawn =="
PROMPT1="Spell the word PINE backwards in uppercase letters and reply with only that."
CTL message "$CHANNEL" "$PROMPT1"

check "untrusted folder: trust dialog surfaced as an approval message" \
  wait_for 90 CTL component "Trust this folder" 0
if TRUST_ID="$(CTL component 'Trust this folder' 0 2>/dev/null)"; then
  CTL click "$CHANNEL" "$TRUST_ID"
  note "clicked Approve on the trust approval"
fi

check "channel message spawned a session and a thread" wait_for 120 CTL thread
THREAD="$(CTL thread 2>/dev/null || true)"
note "thread id: ${THREAD:-<none>}"
if [ -z "$THREAD" ]; then
  echo "FAIL  no thread; cannot continue"
  fail=$((fail + 1))
  exit 1
fi
check "exactly one thread created" test "$(CTL threads)" = 1

check "prompt echoed into the thread" wait_for 60 count_ge "$THREAD" "**You:** $PROMPT1" 1
check "assistant reply mirrored into the thread (ENIP)" wait_for 90 non_echo_has "$THREAD" "ENIP"
check "turn completed and was reported" wait_turns "$THREAD" 1 90

TX="$(transcript_files | head -1)"
SID="$(basename "${TX:-none}" .jsonl)"
SIDS+=("$SID")
note "session id: $SID"
check "exactly one transcript" test "$(transcript_files | wc -l)" = 1
check "its name is a UUID session id" test "${#SID}" = 36
check "the first prompt is the Discord message" grep -qF "$PROMPT1" "$TX"

# ── 2. a 2-line reply injected into the thread arrives as ONE prompt ─────────
echo "== multi-line =="
LINE1="Line one: the secret word is KIWI."
LINE2="Line two: reply with only that secret word in lowercase."
CTL message "$THREAD" "$LINE1"$'\n'"$LINE2"
check "2-line reply echoed" wait_for 60 count_ge "$THREAD" "**You:** $LINE1" 1
check "2-line reply answered (kiwi)" wait_for 90 non_echo_has "$THREAD" "kiwi"
check "second turn completed" wait_turns "$THREAD" 2 90
multi_prompt_count() {
  prompts_of "$TX" | python3 -I -c '
import json, sys
a, b = sys.argv[1], sys.argv[2]
n = sum(1 for l in sys.stdin if a in json.loads(l) and b in json.loads(l) and "\n" in json.loads(l))
print(n)' "$LINE1" "$LINE2"
}
check "both lines are ONE prompt in the transcript" test "$(multi_prompt_count)" = 1
check "no line was submitted as a prompt of its own" \
  test "$(prompts_of "$TX" | python3 -I -c '
import json, sys
a, b = sys.argv[1], sys.argv[2]
print(sum(1 for l in sys.stdin if json.loads(l).strip() in (a, b)))' "$LINE1" "$LINE2")" = 0

# ── 3. a Bash permission prompt produces an approval; Approve lets it run ────
echo "== permission =="
rm -f "$PROJ/live-marker.txt"
CTL message "$THREAD" "Use the Bash tool to run exactly this command: touch live-marker.txt   Then reply with only the word done."
check "permission prompt produced an approval message" wait_for 120 CTL component "Permission required" 0
check "one approval message (no duplicate for the same prompt)" test "$(CTL components 'Permission required')" = 1
check "command has not run before approval" test ! -e "$PROJ/live-marker.txt"
click_approve "$THREAD" "Permission required"
check "Approve let the command run" wait_for 60 test -e "$PROJ/live-marker.txt"
check "third turn completed" wait_turns "$THREAD" 3 120
check "still one approval message after the turn" test "$(CTL components 'Permission required')" = 1

# ── 4. kill the pane, reply: the SAME session id is resumed ──────────────────
echo "== resume =="
live_sessions() { tmux list-sessions -F '#{session_name}' 2>/dev/null | grep '^llm-'; }
OLD="$(live_sessions | head -1)"
note "killing tmux session ${OLD:-<none>}"
tmux kill-session -t "=$OLD" 2>/dev/null
check "pane is gone" test -z "$(live_sessions)"
CTL message "$THREAD" "Spell the word LOOP backwards in uppercase letters and reply with only that."
check "a new pane was started with --resume <same session id>" \
  wait_for 90 bash -c "tmux list-panes -a -F '#{pane_start_command}' | grep -F -- '--resume' | grep -qF '$SID'"
check "resumed session answered in the same thread (POOL)" wait_for 120 non_echo_has "$THREAD" "POOL"
check "resumed prompt landed in the same transcript" wait_for 30 grep -qF "LOOP" "$TX"
check "no second transcript or session was created" test "$(transcript_files | wc -l)" = 1
check "no second thread was created" test "$(CTL threads)" = 1
check "resume turn completed" wait_turns "$THREAD" 4 90
note "transcripts: $(transcript_files | xargs -n1 basename | tr '\n' ' ')"

# ── 5. llmux --resume with an unknown uuid is detected as a refusal ──────────
echo "== refusal =="
BOGUS="$(python3 -I -c 'import uuid; print(uuid.uuid4())')"
# A tmux pane whose only process is the exiting claude loses the message when
# the pane dies, so this launch goes through a shim that runs the real claude,
# reports its exit status and keeps the pane open for a moment.
# RELAY_MARKER_OWNER keeps the relay from claiming this launch's marker.
cat >"$WORK/claude-keep.sh" <<SHIM
#!/bin/sh
claude "\$@"
echo "[claude exited \$?]"
sleep 60
SHIM
chmod +x "$WORK/claude-keep.sh"
RSESS="$(cd "$PROJ" && RELAY_STATE_DIR="$STATE2" RELAY_MARKER_OWNER=live-smoke \
  RELAY_AGENT_CMD="$WORK/claude-keep.sh --model haiku --effort low --permission-mode default" \
  "$BIN/llmux" --resume "$BOGUS" 2>"$WORK/llmux-refusal.log")"
check "llmux --resume <unknown uuid> started a pane" test -n "$RSESS"
pane_has() { tmux capture-pane -p -t "=$RSESS:0.0" 2>/dev/null | grep -qF "$1"; }
check "claude exited" wait_for 60 pane_has "[claude exited 1]"
PANE_TEXT="$(tmux capture-pane -p -t "=$RSESS:0.0" 2>/dev/null)"
note "pane: $(echo "$PANE_TEXT" | grep -v '^$' | head -2 | tr '\n' ' ')"
check "pane shows 'No conversation found with session ID: <id>'" \
  grep -qF "No conversation found with session ID: $BOGUS" <<<"$PANE_TEXT"
check "the relay's own parser classifies it as a refusal" \
  bash -c "cd '$ROOT' && go run ./test/live/refusal" <<<"$PANE_TEXT"
check "marker carries the unknown id as a resume" \
  grep -q "\"sessionID\":\"$BOGUS\".*\"resume\":true\|\"resume\":true.*\"sessionID\":\"$BOGUS\"" "$STATE2"/pending-*
tmux kill-session -t "=$RSESS" 2>/dev/null

echo
echo "== result: $pass passed, $fail failed =="
if [ "$fail" -gt 0 ]; then
  printf '   failed: %s\n' "${failed[@]}"
  exit 1
fi
exit 0
