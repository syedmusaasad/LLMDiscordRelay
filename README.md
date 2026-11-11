# LLMDiscordRelay

LLMDiscordRelay mirrors Claude Code sessions running in tmux to private Discord threads and relays replies back to the terminal.

The local tmux session acts as the source of truth. Discord provides a remote monitor and an external input interface. All execution, file modifications, and agent state reside on your host machine inside tmux. If the relay service stops or disconnects, your terminal session continues running without disruption.

```
+------------------------------------------------------------------------+
|                              Host Machine                              |
|                                                                        |
|  +--------------------+         read-only          +----------------+  |
|  |     Claude Code    | -------------------------> |  JSONL Project |  |
|  |    (inside tmux)   |                            |  Transcripts   |  |
|  +--------------------+                            +----------------+  |
|         ^          |                                       |           |
| send-   |          | capture-pane                          | tail      |
| keys    |          v (readiness/prompts)                   v events    |
|  +------------------------------------------------------------------+  |
|  |                     LLMDiscordRelay Daemon                       |  |
|  |                                                                  |  |
|  |   - Tails transcript events from JSONL session files             |  |
|  |   - Inspects pane status with capture-pane                       |  |
|  |   - Surfaces trust dialogs and approvals to Discord              |  |
|  |   - Filters Discord events by DISCORD_USER_ID                    |  |
|  +------------------------------------------------------------------+  |
|                                |                                       |
|             outbound HTTPS REST and Gateway WebSocket                  |
+--------------------------------|---------------------------------------+
                                 v
+------------------------------------------------------------------------+
|                      Discord Cloud (Private Guild)                     |
|                                                                        |
|  Channel: DISCORD_CHANNEL_ID                                           |
|    |                                                                   |
|    +-- Thread: <session-id> (session mirror + user replies)            |
+------------------------------------------------------------------------+
```

## Transcript vs Pane

The relay separates session content from operational state. The daemon tails transcript events from JSONL files and inspects interactive state from tmux.

Claude Code stores conversation history in JSONL files located at `$CLAUDE_CONFIG_DIR/projects/<project-slug>/<session-id>.jsonl` (default: `~/.claude/projects/<project-slug>/<session-id>.jsonl`). Claude Code creates this file when the first prompt is submitted in the session. The relay opens the JSONL transcript with read-only access to stream user prompts, assistant responses, tool calls, and tool results. The relay never writes to transcript files.

Operational state relies strictly on tmux `capture-pane`. The relay never parses conversational prose from the terminal pane. Instead, the daemon invokes `capture-pane` to detect readiness markers, directory trust dialogs, permission approvals, and interactive option pickers. State detection belongs to `capture-pane`, while conversation content belongs to the JSONL transcript files.

## Trust Dialog and Interactive Prompts

Claude Code displays interactive prompts during operation:

- Directory trust check: When launched in an untrusted directory, Claude Code displays a safety prompt: "Quick safety check: Is this a project you created or one you trust?". The relay forwards this to Discord as an interactive message with Approve and Deny buttons. Clicking Approve sends Down and Enter keys to accept folder trust. Clicking Deny (or allowing the request to time out) sends Escape. The relay never auto-accepts trust dialogs.
- Permission prompts: When Claude Code requests permission to run a command or access a tool, the relay displays the action block in Discord with Approve and Deny buttons. Clicking Approve sends "1", while clicking Deny (or timing out) sends Escape.
- AskUserQuestion pickers: When Claude Code presents an interactive question prompt, the relay parses the options and presents selectable buttons in Discord.

## Discord Structural Difference and Privacy

Discord bots cannot create message threads inside Direct Message channels. Threads require a guild channel. For this reason, LLMDiscordRelay requires a dedicated private server owned by you.

This structural constraint defines a privacy boundary. Inviting the bot into a multi-user server exposes your prompts, assistant replies, and tool execution logs to other server members. You must run the bot in a private server where your account is the sole member.

## Security Posture

LLMDiscordRelay maintains the following security boundaries:

- Local execution under user account: The daemon runs as an unprivileged systemd user service (`llm-discord-relay.service`). It inherits your account's access permissions and does not escalate privileges.
- Configuration security: Credentials reside in `~/.config/llm-discord-relay/.env` outside source control. The environment variable `LLM_RELAY_ENV` allows specifying an alternate path. The installer preserves this configuration during updates.
- State storage: Session state and pending markers reside in `~/.llm-discord-relay`.
- Single-user gating: The relay exits immediately if `DISCORD_USER_ID` is unset or empty. The daemon drops events originating from other users or bots at the gateway boundary.
- Outbound network traffic only: The daemon establishes outbound TLS connections to the Discord Gateway and REST API. It opens no listening network ports on the host.
- Restricted telemetry: Telemetry is disabled by default. If `TELEMETRY_URL` is set, the daemon sends at most one HTTP POST per UTC day containing only `sha256(username)[:12]` and the relay version string.

## Usage

Start a mirrored Claude Code session with `llmux`:

```bash
llmux
```

Pass arguments or an initial prompt directly:

```bash
llmux "Explain this repository architecture"
```

Resume an existing session using its identifier:

```bash
llmux --resume 11111111-2222-4333-8444-555555555555
```

List active sessions tracked by the relay:

```bash
llmux ls
```

Configure the agent command by setting `RELAY_AGENT_CMD` (default: `claude`):

```bash
export RELAY_AGENT_CMD="claude"
# Alternatively, specify a wrapper or alternative agent:
export RELAY_AGENT_CMD="pl"
```

`llmux` passes its caller's `PATH` to the agent (tmux would otherwise give the agent the tmux server's stored environment). Sessions started by the relay service from Discord get the service's `PATH` instead: the unit sets `PATH=~/.local/bin:/usr/local/bin:/usr/bin:/bin`. To extend it, add a `PATH=` line to `~/.config/llm-discord-relay/.env`; `EnvironmentFile` values override the unit's `Environment=PATH` (verified). Write the complete value you want, then run `systemctl --user restart llm-discord-relay.service`.

## Migration from Legacy Versions

When upgrading from pre-1.0 relay releases:

- Configuration migration: On initial startup, if `~/.config/llm-discord-relay/.env` does not exist, the daemon automatically copies your existing `.env` from the previous configuration directory. The original file remains untouched.
- Service switch: Stop and disable the retired systemd user service before enabling the new service:

```bash
systemctl --user disable --now <legacy-service-name>.service
systemctl --user daemon-reload
systemctl --user enable --now llm-discord-relay.service
```
