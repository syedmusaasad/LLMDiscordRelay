# Contributing Guide

Build, test, and verify LLMDiscordRelay using isolated local and containerized harnesses.

## Test Commands

Run the test suite using Makefile targets:

- `make unit`: Runs unit tests locally using `go test -p 1 ./...`.
- `make integration`: Builds the test container and executes integration tests inside Docker via `docker compose -f docker-compose.test.yml run --rm test`.
- `make test`: Executes the complete test sequence by running `make unit` followed by `make integration`.
- `make restart-survival-host`: Runs the daemon lifecycle survival test against the reachable host systemd user manager.

Filter specific integration tests with the `T=` parameter:

```bash
make integration T=TestInject_ReadinessProbe
```

You can pass any regular expression or test function name to `T=`.

## Test Isolation Rules

All automated tests adhere to strict environment isolation boundaries:

- Tests never touch the real HOME directory. Integration tests execute inside Docker as non-root user `testuser` with throwaway `$HOME=/home/testuser`.
- Tests never touch real Discord services. Discord API calls connect to the local mock Discord double. No outbound network requests reach Discord public servers.
- Transcripts use isolated paths. Tests set `CLAUDE_CONFIG_DIR` or `RELAY_CLAUDE_PROJECTS` to temporary directories, leaving production transcripts untouched.

## Fake Agent Test Knobs

The `cmd/fake-claude` binary replicates Claude Code CLI behavior for deterministic testing. Control its behavior using the following environment variables:

- `FAKE_CLAUDE_REFUSE_FIRST_RESUME=1`: Refuses `--resume` launches with exit code 1 and error `No conversation found with session ID: <id>`.
- `FAKE_CLAUDE_READY_DELAY_MS=<n>`: Delays displaying the ready signature by the specified milliseconds.
- `FAKE_CLAUDE_DROP_FIRST_PROMPT=1`: Simulates a dropped prompt to exercise prompt echo retry logic.
- `FAKE_CLAUDE_SWALLOW_UNTIL_READY=1`: Drops keystrokes received before the prompt reaches readiness.
- `FAKE_CLAUDE_PERMISSION_PROMPT=1`: Renders interactive permission approval prompts in the pane.
- `FAKE_CLAUDE_QUESTION_PROMPT=1`: Renders AskUserQuestion interactive menus in the pane.
- `FAKE_CLAUDE_TRUST_DIALOG=1`: Renders the directory trust safety check dialog in the pane.

## Mock Discord Control Endpoints

The `cmd/mock-discord` binary provides a test double for Discord Gateway and REST API operations. The server exposes control endpoints over HTTP on port 9876:

- `POST /control/dispatch`: Injects a Discord gateway event, such as `MESSAGE_CREATE` or `INTERACTION_CREATE`. The endpoint holds dispatch until the connected relay completes IDENTIFY, receives READY, and begins heartbeating.
- `GET /control/sent`: Returns a JSON array of all REST requests sent by the relay.
- `GET /control/calls`: Returns detailed call logs containing HTTP method, path, channel identifier, message identifier, and body.
- `POST /control/ratelimit`: Configures synthetic HTTP 429 rate limit responses for a given endpoint path with a specified `retry_after` duration and response count.
- `GET /control/reqcount`: Retrieves total request counts received across endpoint paths.
- `DELETE /control/sent`: Clears captured request history between test assertions.
