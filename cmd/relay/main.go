// cmd/relay: main entry point for the LLMDiscordRelay.
//
// Configuration via .env file resolved:
//
//	$LLM_RELAY_ENV → ~/.config/llm-discord-relay/.env → ./.env
//
// Required env vars:
//
//	DISCORD_TOKEN       Bot token
//	DISCORD_CHANNEL_ID  Channel to post session threads into
//	DISCORD_USER_ID     The one Discord user whose messages are honoured
//
// Optional env vars:
//
//	RELAY_CLAUDE_PROJECTS  Claude Code projects dir (default: $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects)
//	RELAY_STATE_DIR     State dir (default: ~/.llm-discord-relay)
//	RELAY_LLMUX_BIN     Path to the llmux binary (default: llmux on PATH)
//	RELAY_AGENT_CMD     Agent command llmux runs (default: claude)
//	DISCORD_API_URL     Override Discord API URL (for testing)
//	APPROVAL_TIMEOUT_SECONDS  Auto-deny timeout for pane permission prompts (default 300)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/mirror"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/telemetry"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/tmuxctl"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if err := runRelay(ctx); err != nil {
		log.Fatalf("relay: %v", err)
	}
}

func runRelay(ctx context.Context) error {
	// Load .env config
	if err := loadEnv(); err != nil {
		log.Printf("relay: .env load: %v (continuing with environment)", err)
	}

	token := os.Getenv("DISCORD_TOKEN")
	channelID := os.Getenv("DISCORD_CHANNEL_ID")
	if token == "" || channelID == "" {
		return fmt.Errorf("DISCORD_TOKEN and DISCORD_CHANNEL_ID must be set")
	}

	userID := os.Getenv("DISCORD_USER_ID")
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("DISCORD_USER_ID must be set: refusing to start without a single allowed Discord user")
	}

	projectsRoot, err := transcript.DefaultRoot()
	if err != nil {
		return err
	}

	stateDir, err := resolveStateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}

	// Create Discord client
	apiURL := os.Getenv("DISCORD_API_URL")
	dc, err := newDiscordGatewayClient(token, apiURL)
	if err != nil {
		return fmt.Errorf("discord client: %w", err)
	}

	cfg := mirror.Config{
		DiscordToken:  token,
		DiscordAPIURL: apiURL,
		ChannelID:     channelID,
		AllowedUserID: userID,
		ProjectsRoot:  projectsRoot,
		StateDir:      stateDir,
		LlmuxBin:      os.Getenv("RELAY_LLMUX_BIN"), // optional override for llmux binary path
	}
	if v := os.Getenv("SESSION_TTL_DAYS"); v != "" {
		if days, parseErr := strconv.Atoi(v); parseErr == nil && days > 0 {
			cfg.SessionTTL = time.Duration(days) * 24 * time.Hour
		}
	}
	go telemetry.Run(ctx, stateDir, relayVersion, os.Getenv("TELEMETRY_URL"))
	if v := os.Getenv("RELAY_SPAWN_BIND_TIMEOUT_MS"); v != "" {
		if ms, parseErr := strconv.Atoi(v); parseErr == nil && ms > 0 {
			cfg.SpawnBindTimeout = time.Duration(ms) * time.Millisecond
		}
	}
	if v := os.Getenv("APPROVAL_TIMEOUT_SECONDS"); v != "" {
		if seconds, parseErr := strconv.Atoi(v); parseErr == nil && seconds > 0 {
			cfg.ApprovalTimeout = time.Duration(seconds) * time.Second
		}
	}

	w, err := mirror.NewWatcher(cfg, dc)
	if err != nil {
		return fmt.Errorf("watcher: %w", err)
	}

	// Gateway readiness: the ready file under the state dir is written on the
	// gateway READY event and removed on disconnect, so tests and operators can
	// observe whether inbound events will be consumed.
	readyPath := filepath.Join(stateDir, gatewayReadyFile)
	_ = os.Remove(readyPath)
	markReady := func(event string) {
		if err := os.WriteFile(readyPath, []byte(strconv.FormatInt(time.Now().UnixMilli(), 10)+"\n"), 0644); err != nil {
			log.Printf("relay: write ready file: %v", err)
		}
		log.Printf("relay: gateway ready (%s)", event)
	}
	dc.s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Ready) { markReady("READY") })
	// discordgo reconnects on its own after a dropped connection and RESUMEs;
	// the ready file must come back when inbound events flow again.
	dc.s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) { markReady("RESUMED") })
	dc.s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) {
		_ = os.Remove(readyPath)
		log.Printf("relay: gateway disconnected")
	})

	// Open the gateway connection to receive MESSAGE_CREATE / INTERACTION_CREATE.
	// A failed Open is retried with backoff; if it never succeeds the relay exits
	// non-zero rather than running deaf to inbound events.
	if err := openGateway(ctx, dc.s.Open); err != nil {
		return fmt.Errorf("discord gateway open: %w", err)
	}
	defer dc.s.Close()

	log.Printf("relay: tmux isolation mode: %s", tmuxctl.IsolationMode())
	log.Println("relay: starting watcher")
	w.Run(ctx)
	log.Println("relay: shutting down")
	return nil
}

const relayVersion = "dev"

// gatewayReadyFile is written under RELAY_STATE_DIR when the gateway is READY.
const gatewayReadyFile = "gateway-ready"

// openGateway calls open, retrying with backoff; returns the last error if
// every attempt fails or ctx is cancelled.
func openGateway(ctx context.Context, open func() error) error {
	var err error
	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= 6; attempt++ {
		if err = open(); err == nil {
			return nil
		}
		log.Printf("relay: discord gateway open attempt %d failed: %v", attempt, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}

// ── Discord client adapter ─────────────────────────────────────────────────────

// discordAdapter adapts discordgo.Session to mirror.DiscordClient.
type discordAdapter struct {
	s *discordgo.Session
}

func newDiscordGatewayClient(token, apiURL string) (*discordAdapter, error) {
	if apiURL != "" {
		// Override all package-level endpoint vars that were computed at init time.
		// Updating only EndpointAPI is insufficient because EndpointChannels and other
		// vars are initialized from the old EndpointAPI value.
		discordgo.EndpointDiscord = apiURL + "/"
		discordgo.EndpointAPI = apiURL + "/api/v" + discordgo.APIVersion + "/"
		// Re-derive vars that are read directly (not via functions).
		discordgo.EndpointChannels = discordgo.EndpointAPI + "channels/"
		discordgo.EndpointGateway = discordgo.EndpointAPI + "gateway"
		discordgo.EndpointGuilds = discordgo.EndpointAPI + "guilds/"
		discordgo.EndpointUsers = discordgo.EndpointAPI + "users/"
		discordgo.EndpointWebhooks = discordgo.EndpointAPI + "webhooks/"
	}
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	// Disable discordgo's internal rate-limit retry so *discordgo.RateLimitError
	// is returned to callers.  The relay handles 429 back-off itself in discordSend
	// and discordEdit, feeding the result into the Throttle for future pacing.
	s.ShouldRetryOnRateLimit = false
	return &discordAdapter{s: s}, nil
}

func (d *discordAdapter) SendMessage(channelID, content string) (mirror.MsgRef, error) {
	msg, err := d.s.ChannelMessageSend(channelID, content)
	if err != nil {
		return mirror.MsgRef{}, err
	}
	return mirror.MsgRef{ID: msg.ID, ChannelID: msg.ChannelID}, nil
}

func (d *discordAdapter) EditMessage(channelID, messageID, content string) error {
	_, err := d.s.ChannelMessageEdit(channelID, messageID, content)
	return err
}

func (d *discordAdapter) CreateThread(channelID, messageID, name string) (mirror.ThreadRef, error) {
	ch, err := d.s.MessageThreadStartComplex(channelID, messageID, &discordgo.ThreadStart{
		Name: name,
		Type: discordgo.ChannelTypeGuildPublicThread,
	})
	if err != nil {
		return mirror.ThreadRef{}, err
	}
	return mirror.ThreadRef{ID: ch.ID}, nil
}

func (d *discordAdapter) SendTyping(channelID string) error {
	return d.s.ChannelTyping(channelID)
}

func (d *discordAdapter) RenameThread(threadID, name string) error {
	_, err := d.s.ChannelEdit(threadID, &discordgo.ChannelEdit{Name: name})
	return err
}

func (d *discordAdapter) OnMessage(fn mirror.MessageHandler) {
	d.s.AddHandler(func(_ *discordgo.Session, ev *discordgo.MessageCreate) {
		if ev.Message == nil {
			return
		}
		me := mirror.MessageEvent{
			ChannelID: ev.ChannelID,
			Content:   ev.Content,
		}
		if ev.Author != nil {
			me.AuthorID = ev.Author.ID
			me.AuthorIsBot = ev.Author.Bot
		}
		for _, att := range ev.Attachments {
			if att != nil {
				me.Attachments = append(me.Attachments, mirror.MessageAttachment{
					Filename: att.Filename,
					URL:      att.URL,
				})
			}
		}
		fn(me)
	})
}

func (d *discordAdapter) SendComponents(channelID, content string, components []mirror.Component) (mirror.MsgRef, error) {
	msg, err := d.s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Content: content, Components: discordComponents(components)})
	if err != nil {
		return mirror.MsgRef{}, err
	}
	return mirror.MsgRef{ID: msg.ID, ChannelID: msg.ChannelID}, nil
}

func (d *discordAdapter) EditComponents(channelID, messageID, content string, components []mirror.Component) error {
	edited := discordgo.NewMessageEdit(channelID, messageID).SetContent(content)
	dc := discordComponents(components)
	edited.Components = &dc
	_, err := d.s.ChannelMessageEditComplex(edited)
	return err
}

func (d *discordAdapter) OnInteraction(fn mirror.InteractionHandler) {
	d.s.AddHandler(func(_ *discordgo.Session, ev *discordgo.InteractionCreate) {
		if ev == nil || ev.Type != discordgo.InteractionMessageComponent {
			return
		}
		data := ev.MessageComponentData()
		interaction := mirror.InteractionEvent{ID: ev.ID, Token: ev.Token, ChannelID: ev.ChannelID, CustomID: data.CustomID, Values: data.Values}
		if ev.Member != nil && ev.Member.User != nil {
			interaction.AuthorID = ev.Member.User.ID
			interaction.IsBot = ev.Member.User.Bot
		} else if ev.User != nil {
			interaction.AuthorID = ev.User.ID
			interaction.IsBot = ev.User.Bot
		}
		fn(interaction)
	})
}

func (d *discordAdapter) AcknowledgeInteraction(ev mirror.InteractionEvent) error {
	return d.s.InteractionRespond(&discordgo.Interaction{ID: ev.ID, Token: ev.Token}, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})
}

func discordComponents(components []mirror.Component) []discordgo.MessageComponent {
	if len(components) == 0 {
		return []discordgo.MessageComponent{}
	}
	row := discordgo.ActionsRow{}
	for _, component := range components {
		switch component.Type {
		case "button":
			style := discordgo.PrimaryButton
			if component.Style == "success" {
				style = discordgo.SuccessButton
			} else if component.Style == "danger" {
				style = discordgo.DangerButton
			}
			row.Components = append(row.Components, discordgo.Button{CustomID: component.CustomID, Label: component.Label, Style: style})
		case "select":
			options := make([]discordgo.SelectMenuOption, 0, len(component.Options))
			for _, option := range component.Options {
				options = append(options, discordgo.SelectMenuOption{Label: option.Label, Value: option.Value})
			}
			row.Components = append(row.Components, discordgo.SelectMenu{CustomID: component.CustomID, Placeholder: component.Placeholder, Options: options, MaxValues: 1})
		}
	}
	return []discordgo.MessageComponent{row}
}

// ── Config helpers ─────────────────────────────────────────────────────────────

// loadEnv loads a .env file. Resolution order:
//  1. $LLM_RELAY_ENV
//  2. ~/.config/llm-discord-relay/.env (seeded from the pre-1.0 config directory
//     when it does not exist yet; see migrateLegacyEnv)
//  3. ./.env
func loadEnv() error {
	var paths []string
	if v := os.Getenv("LLM_RELAY_ENV"); v != "" {
		paths = append(paths, v)
	}
	if home, err := os.UserHomeDir(); err == nil {
		current := filepath.Join(home, ".config", configDirName, ".env")
		if migrated, err := migrateLegacyEnv(home); err != nil {
			log.Printf("relay: migrate previous config: %v", err)
		} else if migrated {
			log.Printf("relay: copied %s to %s (the previous file was left in place)", legacyEnvPath(home), current)
		}
		paths = append(paths, current)
	}
	paths = append(paths, ".env")

	for _, p := range paths {
		if err := readDotEnv(p); err == nil {
			return nil
		}
	}
	return fmt.Errorf("no .env found in %v", paths)
}

// readDotEnv reads a .env file and sets environment variables (does not overwrite).
func readDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// Remove surrounding quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
	return nil
}

// resolveStateDir returns the relay state directory.
func resolveStateDir() (string, error) {
	if v := os.Getenv("RELAY_STATE_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".llm-discord-relay"), nil
}
