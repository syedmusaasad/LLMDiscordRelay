# Setup Guide

Configure a Discord bot, a private server channel, and environment variables to deploy LLMDiscordRelay.

## 1. Create Discord Application

Open `https://discord.com/developers/applications` in a web browser. Click "New Application" in the top right corner. Enter an application name and confirm the dialog.

## 2. Configure the Bot User and Retrieve Token

Select the "Bot" tab from the left navigation menu. Click "Reset Token" to generate a token string. Copy the token to a secure location. This value supplies `DISCORD_TOKEN`.

## 3. Enable Message Content Intent

Scroll down the Bot page to the "Privileged Gateway Intents" section. Enable the `MESSAGE CONTENT INTENT` switch. Save changes. The relay requires message content access to read thread messages and forward input to tmux panes.

## 4. Create a Private Server

Open your Discord client and create a dedicated personal server. Retain sole membership so no other users join. Create a text channel for relay sessions, such as `#relay`.

## 5. Generate Invite URL and Authorize Bot

Return to the Discord Developer Portal and select "OAuth2", then open "URL Generator".

Under the "Scopes" list, select `bot`.

Under the "Bot Permissions" list, select the following permissions:
- Send Messages
- Create Public Threads
- Send Messages in Threads
- Manage Threads
- Read Message History
- Attach Files
- Embed Links

Copy the generated URL displayed at the bottom of the page. Open the URL in your browser. Select your private server from the dropdown menu and authorize the bot.

## 6. Collect Channel ID and User ID

Open Discord User Settings, select "Advanced", and enable Developer Mode.

Right-click your designated relay text channel in the server sidebar. Select "Copy Channel ID". This numerical value supplies `DISCORD_CHANNEL_ID`.

Right-click your personal user profile in the channel member list. Select "Copy User ID". This numerical value supplies `DISCORD_USER_ID`. The daemon drops events from every other user.

## 7. Write Environment Configuration

Create the configuration file at `~/.config/llm-discord-relay/.env`. The systemd user service reads this file via `EnvironmentFile`. You can override the path by setting `LLM_RELAY_ENV`.

`DISCORD_USER_ID` is required: the relay terminates immediately if it is unset or empty.

If you are upgrading from a pre-1.0 release, the daemon automatically copies your existing `.env` configuration file to `~/.config/llm-discord-relay/.env` on first startup if the destination does not already exist.

```env
# Required credentials
DISCORD_TOKEN=your_bot_token_here
DISCORD_CHANNEL_ID=123456789012345678
DISCORD_USER_ID=987654321098765432

# Optional runtime controls
# RELAY_AGENT_CMD=claude
# TELEMETRY_URL=   # unset = no telemetry
APPROVAL_TIMEOUT_SECONDS=300
SESSION_TTL_DAYS=30
```

## 8. Install and Start the Service

If upgrading from a pre-1.0 relay release, stop and disable the previous user service:

```bash
systemctl --user disable --now <legacy-service-name>.service
```

Execute the installer script from the root of the repository:

```bash
./install.sh
```

The script builds the binaries, installs them to `~/.local/bin/relay` and `~/.local/bin/llmux`, installs the systemd user service to `~/.config/systemd/user/llm-discord-relay.service`, and starts the service.

Verify that the service is running:

```bash
systemctl --user status llm-discord-relay.service
```
