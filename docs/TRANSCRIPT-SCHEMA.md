# Transcript Schema

Claude Code records session history in JSON Lines format under the user projects directory.

## File Location and Lifecycle

Transcripts reside at:

```
$CLAUDE_CONFIG_DIR/projects/<project-slug>/<session-id>.jsonl
```

The default directory is `~/.claude/projects/<project-slug>/<session-id>.jsonl`. The `<project-slug>` is derived by replacing filesystem separators in the working directory path with hyphens (for example, `/root/demo` becomes `-root-demo`).

The session identifier is a UUIDv4 string (for example, `11111111-2222-4333-8444-555555555555`).

Claude Code creates the transcript file only after the first prompt is submitted. Launching the process does not create the file. The relay opens transcript files in read-only mode and tails appended lines. The relay never writes to transcript files.

## Top-Level Line Structure

Each line in the file is a single JSON object. The relay inspects the following fields:

```json
{
  "type": "user",
  "subtype": "",
  "uuid": "4fa86576-59b1-4f16-92d4-046646b9a896",
  "timestamp": "2026-11-08T00:00:01.000000000Z",
  "isSidechain": false,
  "isMeta": false,
  "cwd": "/root/demo",
  "aiTitle": "",
  "message": {}
}
```

- `type`: Category of the record (`user`, `assistant`, `system`, or `ai-title`).
- `subtype`: Sub-classification of the record, such as `turn_duration` for system events.
- `uuid`: Unique identifier for the transcript line.
- `timestamp`: RFC 3339 nano timestamp.
- `isSidechain`: Boolean indicating whether the event belongs to an internal subagent. The relay ignores sidechain lines.
- `isMeta`: Boolean indicating harness metadata. The relay ignores meta lines.
- `cwd`: Absolute path of the session working directory.
- `aiTitle`: AI-generated conversation summary when `type` is `ai-title`.
- `message`: Object holding role, model, and message content blocks.

## Message Object Structure

The `message` property contains payload data:

```json
{
  "id": "msg_01ABC...",
  "role": "assistant",
  "model": "claude-3-5-sonnet-20241022",
  "content": []
}
```

- `id`: Assistant message identifier that groups multiple content blocks belonging to one API turn.
- `role`: Message role (`user` or `assistant`).
- `model`: Model name used to generate the assistant turn.
- `content`: Either a plain JSON string (for simple user prompts) or an array of structured blocks.

## Content Block Types

Structured `content` arrays contain one or more typed blocks:

### 1. Text Block
```json
{
  "type": "text",
  "text": "Hello, world!"
}
```

### 2. Tool Call Block
```json
{
  "type": "tool_use",
  "id": "toolu_01ABC...",
  "name": "Bash",
  "input": {
    "command": "ls -la"
  }
}
```

### 3. Tool Result Block
```json
{
  "type": "tool_result",
  "tool_use_id": "toolu_01ABC...",
  "content": "total 0\n",
  "is_error": false
}
```

## Turn Boundaries and Event Mapping

The relay transforms transcript lines into internal mirror events:

- User Prompts (`user-prompt`): Extracted from lines where `type` is `user` and `isMeta` is false. The relay filters out harness messages starting with `<command-`, `<local-command-`, `<system-reminder>`, `<bash-`, or `<user-memory-input>`.
- Assistant Text (`assistant-text`): Extracted from text blocks within assistant messages. Blocks sharing a `message.id` belong to the same response.
- Tool Calls (`tool-call`): Extracted from `tool_use` blocks within assistant messages.
- Tool Results (`tool-result`): Extracted from `tool_result` blocks within user lines.
- Turn Completion (`turn-end`): Emitted when a line with `type: "system"` and `subtype: "turn_duration"` is parsed.
- Interrupted Turns: When a turn is interrupted (such as pressing Escape or denying a permission prompt), Claude Code writes a user line beginning with `[Request interrupted by user`. No `turn_duration` line is emitted for interrupted turns; the relay marks the turn complete upon detecting the interrupt prefix.
