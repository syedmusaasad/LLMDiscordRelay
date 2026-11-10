# Session Binding Protocol

LLMDiscordRelay binds tmux terminal panes to Claude Code transcripts using pre-allocated session identifiers and pending state markers.

## Mechanism

The launcher (`llmux`) generates a random UUIDv4 session identifier before starting Claude Code.

1. **Pre-allocated Identity**: The launcher generates a UUIDv4 string (for example, `11111111-2222-4333-8444-555555555555`). New sessions launch with `claude --session-id <uuid>`. Resumed sessions launch with `claude --resume <uuid>`.
2. **Pending Marker Creation**: Before spawning the tmux session, `llmux` writes an atomic JSON marker file at `~/.llm-discord-relay/pending-<session-name>`. The file contains the tmux target, creation timestamp, session identifier, resume flag, and optional owner tag:

```json
{
  "tmux_target": "llm-1762560000-12345:=0",
  "session_id": "11111111-2222-4333-8444-555555555555",
  "created": "2026-11-08T00:00:00.000000000Z",
  "resume": false,
  "owner": ""
}
```

3. **Transcript Creation**: Claude Code creates the transcript file at `$CLAUDE_CONFIG_DIR/projects/<slug>/<session-id>.jsonl` when the first prompt is submitted. The relay monitors the projects directory for the exact session identifier recorded in the pending marker.
4. **Thread Association**: Upon detecting the transcript file, the relay binds the tmux pane to the transcript stream, creates or attaches the Discord thread, and removes the pending marker from disk.
5. **No Stderr Parsing**: Because the session identifier is assigned prior to launch, the binding mechanism requires no stderr redirection, process sniffing, or log parsing flags.

## Resumes and Refusal Detection

When resuming an existing session from Discord or the CLI:

1. `llmux` sets `resume: true` in the pending marker and executes `claude --resume <uuid>`.
2. If the session exists, Claude Code reopens the transcript file and resumes interactive turns.
3. If the session identifier is unknown, Claude Code prints `No conversation found with session ID: <id>` to the pane and exits with code 1.
4. The relay detects the refusal through pane inspection or process termination, removes the stale marker, and falls back to spawning a fresh session when triggered from Discord.
