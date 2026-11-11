// Package mirror: watcher pairs pending spawn markers with Claude Code
// transcripts and mirrors them to Discord.
package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/tmuxctl"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
)

// Config holds relay watcher configuration.
type Config struct {
	// Discord
	DiscordToken  string
	DiscordAPIURL string // override for testing
	ChannelID     string
	AllowedUserID string // only honour messages from this Discord user ID (DISCORD_USER_ID)

	// ProjectsRoot is the Claude Code projects directory holding the session
	// transcripts (default: transcript.DefaultRoot()).
	ProjectsRoot string

	// State
	StateDir string

	// Binaries
	// LlmuxBin is the path to the llmux binary (default: "llmux" on PATH).
	// Used when spawning a new session from a Discord channel message.
	LlmuxBin string

	// Timing
	PollInterval time.Duration // default 1s

	// SpawnBindTimeout is how long to wait for the transcript to appear after
	// the first prompt of a spawned session was sent. Default 60s.
	SpawnBindTimeout time.Duration

	// ApprovalTimeout is the maximum time an approval prompt can remain unanswered.
	// The default is five minutes.
	ApprovalTimeout time.Duration

	// SessionTTL controls retention for dead sessions.json records (default 30d).
	SessionTTL time.Duration
}

// DiscordClient is the interface the watcher uses to talk to Discord.
type DiscordClient interface {
	SendMessage(channelID, content string) (MsgRef, error)
	EditMessage(channelID, messageID, content string) error
	CreateThread(channelID, messageID, name string) (ThreadRef, error)
	SendTyping(channelID string) error
	RenameThread(threadID, name string) error
	// OnMessage registers a callback invoked for every MESSAGE_CREATE gateway event.
	// Multiple handlers may be registered; each receives a copy of the event.
	OnMessage(fn MessageHandler)
	SendComponents(channelID, content string, components []Component) (MsgRef, error)
	EditComponents(channelID, messageID, content string, components []Component) error
	OnInteraction(fn InteractionHandler)
	AcknowledgeInteraction(InteractionEvent) error
}

// Component is a transport-neutral Discord message component.
type Component struct {
	Type        string
	CustomID    string
	Label       string
	Style       string
	Placeholder string
	Options     []ComponentOption
}

// ComponentOption is one selectable value in a select menu.
type ComponentOption struct {
	Label string
	Value string
}

// InteractionEvent is the subset of a component interaction used by the relay.
type InteractionEvent struct {
	ID        string
	Token     string
	ChannelID string
	AuthorID  string
	IsBot     bool
	CustomID  string
	Values    []string
}

// InteractionHandler receives a Discord component interaction.
type InteractionHandler func(InteractionEvent)

// MessageEvent carries the fields of a Discord MESSAGE_CREATE event that the
// inject path needs.
type MessageEvent struct {
	ChannelID   string
	AuthorID    string
	AuthorIsBot bool
	Content     string
	Attachments []MessageAttachment
}

// MessageAttachment is a single file attachment on a Discord message.
type MessageAttachment struct {
	Filename string
	URL      string
}

// MessageHandler is called for every incoming Discord message.
type MessageHandler func(ev MessageEvent)

// MsgRef is a minimal message reference.
type MsgRef struct {
	ID        string
	ChannelID string
}

// ThreadRef is a minimal thread reference.
type ThreadRef struct {
	ID string
}

// Watcher mirrors Claude Code sessions to Discord.
type Watcher struct {
	cfg      Config
	store    transcript.Store
	discord  DiscordClient
	sessions *Sessions
	throttle *Throttle
	injector *inject.Injector
	// runCtx is cancelled when the relay is shutting down.  Discord's REST
	// interface has no context parameter, so the relay-owned 429 wait must use
	// this context rather than an unconditional Sleep.
	runCtx context.Context
	// claimed guards against double-binding a session from two concurrent paths
	// (scanSessions and handleSpawnFromDiscord). A session ID is inserted here
	// the first time it is claimed; subsequent claimants are rejected.
	claimed claimSet
	// pendingResume holds per-thread pending !resume <n> candidate lists.
	pendingResume pendingResumeStore
	// suppressedEchoes tracks (sessionID, promptText) pairs whose Discord echo
	// has already been posted once. A second appearance of the same prompt in
	// the transcript (due to inject retry) must not be posted again.
	suppressedEchoes    suppressedEchoStore
	pendingInteractions pendingInteractionStore
	// capturePane reads a pane for click-time validation; nil means tmux.
	capturePane func(target string) (string, error)
	// mirrors holds the cancel func of the live content mirror per session, so a
	// resumed session's new mirror supersedes the one of its dead pane.
	mirrorsMu sync.Mutex
	mirrors   map[string]*mirrorHandle
}

type mirrorHandle struct{ cancel context.CancelFunc }

// pendingInteractionStore records one outstanding pane interaction per Discord
// component message. It prevents duplicate clicks and lets the timeout race be
// resolved exactly once.
type pendingInteractionStore struct {
	mu       sync.Mutex
	entries  map[string]pendingInteraction
	resolved map[string]time.Time
}

type pendingInteraction struct {
	target    string
	threadID  string
	messageID string
	kind      string
	options   int
	signature string    // the action/question this entry was posted for
	result    chan bool // kind "trust": receives the final decision
}

// interactionGrace is how long a resolved interaction suppresses a re-post of
// an identical prompt: the pane still shows the old prompt until the key lands.
const interactionGrace = 1500 * time.Millisecond

func interactionKey(threadID, target, kind string) string {
	return threadID + "\x00" + target + "\x00" + kind
}

// markResolved records that the interaction of (thread, target, kind) was
// settled (answered, timed out or retired as stale).
func (p *pendingInteractionStore) markResolved(threadID, target, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolved == nil {
		p.resolved = make(map[string]time.Time)
	}
	p.resolved[interactionKey(threadID, target, kind)] = time.Now()
}

// clearResolved forgets a settled marker (a fresh interaction was posted).
func (p *pendingInteractionStore) clearResolved(threadID, target, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.resolved, interactionKey(threadID, target, kind))
}

// settled reports whether the last interaction was resolved at least grace ago,
// i.e. a prompt still on screen with the same signature is a new one.
func (p *pendingInteractionStore) settled(threadID, target, kind string, grace time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.resolved[interactionKey(threadID, target, kind)]
	return ok && time.Since(at) >= grace
}

func (p *pendingInteractionStore) put(id string, entry pendingInteraction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = make(map[string]pendingInteraction)
	}
	p.entries[id] = entry
}

// claim atomically takes ownership of the whole request id belongs to: the
// entry itself and every sibling entry on the same Discord message (the
// Approve/Deny pair), under one lock. Exactly one of any number of concurrent
// callers (button clicks, timeout, retirement) gets ok=true; the rest see
// nothing and must not touch the pane. ids lists the removed custom IDs so a
// failed action can restore them.
func (p *pendingInteractionStore) claim(id string) (entry pendingInteraction, ids []string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok = p.entries[id]
	if !ok {
		return pendingInteraction{}, nil, false
	}
	for sid, e := range p.entries {
		if e.threadID == entry.threadID && e.messageID == entry.messageID {
			delete(p.entries, sid)
			ids = append(ids, sid)
		}
	}
	return entry, ids, true
}

// restore re-arms a claimed request (its action could not be carried out).
func (p *pendingInteractionStore) restore(ids []string, entry pendingInteraction) {
	for _, id := range ids {
		p.put(id, entry)
	}
}

// retire removes every pending entry of (thread, target, kind) that is not
// keyed by the prompt currently on the pane: all of them when the pane shows no
// such prompt (showing=false), otherwise those whose signature differs from
// signature. It returns one entry per Discord message so the caller can mark
// the message expired.
func (p *pendingInteractionStore) retire(threadID, target, kind string, showing bool, signature string) []pendingInteraction {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []pendingInteraction
	seenMsg := make(map[string]bool)
	for id, entry := range p.entries {
		if entry.threadID != threadID || entry.target != target || entry.kind != kind {
			continue
		}
		if showing && entry.signature == signature {
			continue
		}
		delete(p.entries, id)
		if !seenMsg[entry.messageID] {
			seenMsg[entry.messageID] = true
			out = append(out, entry)
		}
	}
	return out
}

func (p *pendingInteractionStore) has(threadID, target, kind string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.entries {
		if entry.threadID == threadID && entry.target == target && entry.kind == kind {
			return true
		}
	}
	return false
}

// echoSuppressWindow is how long after a retried injection a duplicate echo of
// the same prompt is still suppressed.
const echoSuppressWindow = 2 * time.Minute

// suppressedEchoStore de-duplicates the "You: ..." echo of a prompt the relay
// had to inject twice. injectAndVerify arms an entry before the retry; the
// FIRST transcript event for that text is still posted (the first send may have
// been lost, so it can be the only one), and only a SECOND event for the same
// text is suppressed. An entry that never sees a duplicate expires, so a later,
// genuinely repeated prompt is not swallowed.
type suppressedEchoStore struct {
	mu      sync.Mutex
	entries map[string]map[string]*echoEntry // sessionID -> prompt text -> state
}

type echoEntry struct {
	armed  time.Time
	posted bool // the first echo has been let through
}

// add arms duplicate suppression for prompt in sessionID.
func (s *suppressedEchoStore) add(sessionID, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]map[string]*echoEntry)
	}
	if s.entries[sessionID] == nil {
		s.entries[sessionID] = make(map[string]*echoEntry)
	}
	s.entries[sessionID][prompt] = &echoEntry{armed: time.Now()}
}

// suppress reports whether the echo for prompt just seen in sessionID is a
// duplicate that must not be posted. Call it once per prompt event.
func (s *suppressedEchoStore) suppress(sessionID, prompt string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[sessionID][prompt]
	if e == nil {
		return false
	}
	if time.Since(e.armed) > echoSuppressWindow {
		delete(s.entries[sessionID], prompt)
		return false
	}
	if !e.posted {
		e.posted = true
		return false
	}
	delete(s.entries[sessionID], prompt)
	return true
}

// claimSet is a thread-safe set of claimed session IDs.
type claimSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (c *claimSet) init() {
	c.ids = make(map[string]bool)
}

// tryAdd returns true and inserts id if it was not already present.
// Returns false (no-op) if already claimed.
func (c *claimSet) tryAdd(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ids == nil {
		c.ids = make(map[string]bool)
	}
	if c.ids[id] {
		return false
	}
	c.ids[id] = true
	return true
}

// copyInto copies all claimed IDs into dst (non-destructive read).
func (c *claimSet) copyInto(dst map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.ids {
		dst[id] = true
	}
}

// NewWatcher creates a watcher.
func NewWatcher(cfg Config, discord DiscordClient) (*Watcher, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.SpawnBindTimeout == 0 {
		cfg.SpawnBindTimeout = 60 * time.Second
	}
	if cfg.ApprovalTimeout == 0 {
		cfg.ApprovalTimeout = 5 * time.Minute
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.ProjectsRoot == "" {
		root, err := transcript.DefaultRoot()
		if err != nil {
			return nil, fmt.Errorf("mirror: projects root: %w", err)
		}
		cfg.ProjectsRoot = root
	}
	sessPath := filepath.Join(cfg.StateDir, "sessions.json")
	sessions, err := LoadSessions(sessPath)
	if err != nil {
		return nil, fmt.Errorf("mirror: load sessions: %w", err)
	}
	w := &Watcher{
		cfg:      cfg,
		store:    transcript.Store{Root: cfg.ProjectsRoot},
		discord:  discord,
		sessions: sessions,
		throttle: NewThrottle(5, 5*time.Second),
		injector: inject.New(cfg.StateDir),
		runCtx:   context.Background(),
	}
	if err := sessions.SweepExpired(cfg.SessionTTL, isTmuxPaneAlive, time.Now()); err != nil {
		log.Printf("mirror: sessions.json TTL sweep: %v", err)
	}

	// Register incoming message handler for thread replies.
	discord.OnMessage(w.handleDiscordMessage)
	discord.OnInteraction(w.handleInteraction)

	return w, nil
}

// NewWatcherForTest builds a minimal Watcher around a real DiscordClient
// (e.g. wrapping the production discord.New() client) for tests that need to
// exercise the relay's own 429 retry/throttle path (discordSend) without a
// full transcript/tmux/llmux fixture. It uses an in-memory sessions store and
// default throttle settings, matching what NewWatcher configures.
func NewWatcherForTest(discord DiscordClient) (*Watcher, error) {
	return &Watcher{
		discord:  discord,
		sessions: &Sessions{records: make(map[string]*SessionRecord)},
		throttle: NewThrottle(5, 5*time.Second),
		runCtx:   context.Background(),
	}, nil
}

// DiscordSendForTest is the exported form of discordSend used by tests that
// need to exercise the relay's bounded, context-cancellable 429 retry path
// against a real DiscordClient (see discordSend for the actual logic: on a
// rate-limit error it extracts RetryAfter, sleeps, and retries exactly once).
func (w *Watcher) DiscordSendForTest(channelID, content string) (MsgRef, error) {
	return w.discordSend(channelID, content)
}

// Run starts the watcher loop until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	w.runCtx = ctx
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	ttlTicker := time.NewTicker(24 * time.Hour)
	defer ttlTicker.Stop()
	// Track known session IDs to avoid re-pairing
	seen := make(map[string]bool)
	// Pre-seed with already-tracked sessions
	for _, r := range w.sessions.All() {
		seen[r.SessionID] = true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Merge any sessions claimed by handleSpawnFromDiscord into seen.
			w.claimed.copyInto(seen)
			w.scanSessions(ctx, seen)
		case <-ttlTicker.C:
			if err := w.sessions.SweepExpired(w.cfg.SessionTTL, isTmuxPaneAlive, time.Now()); err != nil {
				log.Printf("mirror: sessions.json TTL sweep: %v", err)
			}
		}
	}
}

// spawnMarkerJSON is the JSON pending marker written by llmux. llmux chooses
// the Claude Code session id itself (claude --session-id / --resume), so the
// marker carries the identity directly: nothing has to be inferred from the
// session store, and a transcript with this exact id can only belong to this
// launch. See docs/BINDING-PROTOCOL.md.
type spawnMarkerJSON struct {
	Target       string `json:"target"`
	SpawnInstant int64  `json:"spawnInstant"`
	SessionID    string `json:"sessionID"`
	// Resume marks a marker for `claude --resume <id>`: the transcript already
	// exists, and a pane that is not (yet) alive is expected.
	Resume bool `json:"resume,omitempty"`
	// Owner is non-empty when a relay spawn path (handleSpawnFromDiscord,
	// spawnResume) launched llmux and therefore owns the marker: it decides
	// bind, timeout and cleanup. scanSessions never claims an owned marker.
	Owner string `json:"owner,omitempty"`
	// AgentCmd is the RELAY_AGENT_CMD string llmux used (empty when unset). It
	// is stored on the session record so a resume runs the same agent.
	AgentCmd string `json:"agentCmd,omitempty"`
}

// markerOwnerSpawn is the Owner value llmux records for the relay's own spawn paths.
const markerOwnerSpawn = "relay-spawn"

// staleMarkerAge is how old a marker must be before scanSessions may delete it
// for being orphaned (dead pane, or session already tracked).
const staleMarkerAge = 2 * time.Minute

// pendingMarker is a parsed pending-<name> file.
type pendingMarker struct {
	path string
	mod  time.Time
	spawnMarkerJSON
}

// readPendingMarkers returns the valid pending markers in stateDir. Markers
// without a well-formed session id are ignored: there is no heuristic fallback.
func readPendingMarkers(stateDir string) []pendingMarker {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}
	var out []pendingMarker
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "pending-") || strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		fp := filepath.Join(stateDir, e.Name())
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		var m spawnMarkerJSON
		if json.Unmarshal(data, &m) != nil || m.Target == "" || !transcript.ValidSessionID(m.SessionID) {
			continue
		}
		pm := pendingMarker{path: fp, spawnMarkerJSON: m}
		if info, err := e.Info(); err == nil {
			pm.mod = info.ModTime()
		}
		out = append(out, pm)
	}
	return out
}

// scanSessions pairs pending markers with transcripts. Markers owned by a relay
// spawn path (Owner set) are left alone. A marker whose session
// id already has a transcript (the first prompt was submitted) is claimed and
// mirrored; a new-spawn marker additionally needs its pane alive.
func (w *Watcher) scanSessions(ctx context.Context, seen map[string]bool) {
	for _, m := range readPendingMarkers(w.cfg.StateDir) {
		old := !m.mod.IsZero() && time.Since(m.mod) > staleMarkerAge
		if m.Owner != "" {
			// Exactly one owner per marker: the spawn path that launched it
			// decides bind, bind timeout and cleanup. The scanner only sweeps
			// markers whose owner is long gone (dead pane).
			if old && !isTmuxPaneAlive(m.Target) {
				os.Remove(m.path)
			}
			continue
		}
		if seen[m.SessionID] {
			if old {
				// Tracked via sessions.json but never re-attached: the marker is spent.
				os.Remove(m.path)
			}
			continue
		}
		if _, err := w.store.Path(m.SessionID); err != nil {
			// No first prompt yet. Keep waiting while the pane lives.
			if old && !m.Resume && !isTmuxPaneAlive(m.Target) {
				os.Remove(m.path)
			}
			continue
		}
		if !m.Resume && !isTmuxPaneAlive(m.Target) {
			if old {
				os.Remove(m.path)
			}
			continue
		}
		// Atomically claim this session to prevent double-binding with handleSpawnFromDiscord.
		if !w.claimed.tryAdd(m.SessionID) {
			seen[m.SessionID] = true
			continue
		}
		seen[m.SessionID] = true

		go func(sessionID, tmuxTarget, pendingPath, agentCmd string) {
			defer os.Remove(pendingPath) // clean up marker

			if err := w.mirrorSession(ctx, sessionID, tmuxTarget, agentCmd); err != nil {
				log.Printf("mirror: session %s error: %v", sessionID, err)
			}
		}(m.SessionID, m.Target, m.path, m.AgentCmd)
	}
}

// mirrorSession mirrors a single session to Discord.
func (w *Watcher) mirrorSession(ctx context.Context, sessionID, tmuxTarget, agentCmd string) error {
	log.Printf("mirror: attaching session %s target=%s", sessionID, tmuxTarget)

	// Create thread root message
	rootContent := fmt.Sprintf("🟢 **Session** `%s`\nTarget: `%s`", sessionID, tmuxTarget)
	rootMsg, err := w.discord.SendMessage(w.cfg.ChannelID, rootContent)
	if err != nil {
		return fmt.Errorf("create root message: %w", err)
	}

	// Create thread
	thread, err := w.discord.CreateThread(w.cfg.ChannelID, rootMsg.ID, sessionID)
	if err != nil {
		return fmt.Errorf("create thread: %w", err)
	}

	threadID := thread.ID

	// Persist the session record
	rec := &SessionRecord{
		SessionID:    sessionID,
		TmuxTarget:   tmuxTarget,
		ThreadID:     threadID,
		ThreadRootID: rootMsg.ID,
		ChannelID:    w.cfg.ChannelID,
		Status:       "active",
		AgentCmd:     agentCmd,
	}
	if err := w.sessions.Upsert(rec); err != nil {
		log.Printf("mirror: sessions.json upsert: %v", err)
	}

	// Mirror the session content
	if err := w.mirrorContent(ctx, sessionID, threadID, tmuxTarget, 0); err != nil {
		if errors.Is(err, errMirrorSuperseded) {
			// A resumed pane took over this thread; it is not ending.
			return nil
		}
		log.Printf("mirror: mirrorContent error: %v", err)
	}

	// Post session-ended marker
	endContent := "⏹ Session ended"
	if _, postErr := w.discord.SendMessage(threadID, endContent); postErr != nil {
		log.Printf("mirror: post session-ended: %v", postErr)
	}

	// Flip thread-root emoji
	updatedRoot := fmt.Sprintf("⚫ **Session** `%s`\nTarget: `%s`", sessionID, tmuxTarget)
	if editErr := w.discord.EditMessage(w.cfg.ChannelID, rootMsg.ID, updatedRoot); editErr != nil {
		log.Printf("mirror: flip root emoji: %v", editErr)
	}

	// Keep sessions.json claim (status=ended)
	rec.Status = "ended"
	_ = w.sessions.Upsert(rec)

	return nil
}

// errMirrorSuperseded is returned by mirrorContent when a newer mirror for the
// same session (after a resume) replaced it.
var errMirrorSuperseded = errors.New("mirror: superseded by a newer mirror")

// msgState tracks the Discord messages that stream one assistant message.id.
type msgState struct {
	discordMsgID  string   // current edit-target Discord message ID for assistant text
	discordMsgIDs []string // all Discord message IDs posted for this assistant text
	text          string   // accumulated text so far
}

// toolState tracks per-tool-call state.
type toolState struct {
	callID   string
	toolName string
	msgID    string // Discord message ID for the embed
	done     bool
	failed   bool
}

// turnState tracks the turn currently being mirrored.
type turnState struct {
	open      bool
	hadOutput bool
	tools     []*toolState
}

// registerMirror makes this mirror the live one for sessionID and cancels any
// earlier one. The returned func unregisters it if it is still the live one.
func (w *Watcher) registerMirror(ctx context.Context, sessionID string) (context.Context, func()) {
	mctx, cancel := context.WithCancel(ctx)
	h := &mirrorHandle{cancel: cancel}
	w.mirrorsMu.Lock()
	if w.mirrors == nil {
		w.mirrors = make(map[string]*mirrorHandle)
	}
	if old := w.mirrors[sessionID]; old != nil {
		old.cancel()
	}
	w.mirrors[sessionID] = h
	w.mirrorsMu.Unlock()
	return mctx, func() {
		w.mirrorsMu.Lock()
		if w.mirrors[sessionID] == h {
			delete(w.mirrors, sessionID)
		}
		w.mirrorsMu.Unlock()
		cancel()
	}
}

// mirrorContent tails the session transcript and mirrors it to the Discord
// thread: one Discord message per assistant message.id (edited as blocks
// arrive), one tool embed per tool_use id, and a summary at the end of a turn.
// startOffset skips that many bytes of the transcript (0 replays all of it).
func (w *Watcher) mirrorContent(parent context.Context, sessionID, threadID, tmuxTarget string, startOffset int64) error {
	ctx, release := w.registerMirror(parent, sessionID)
	defer release()

	tailer := transcript.NewTailer(w.store, sessionID, 250*time.Millisecond)
	tailer.StartOffset = startOffset
	ch := tailer.Tail(ctx)

	// Typing ticker
	typingTicker := time.NewTicker(8 * time.Second)
	defer typingTicker.Stop()

	// Liveness check for the tmux target
	livenessTicker := time.NewTicker(5 * time.Second)
	defer livenessTicker.Stop()

	asst := make(map[string]*msgState)   // assistant message.id -> Discord streaming state
	tools := make(map[string]*toolState) // tool_use id -> embed state
	var turn turnState
	seenAsst := make(map[string]bool) // dedupe key: line uuid + block ordinal text
	// Prompt recognition belongs to the pane, not the transcript: approvals have
	// no blocking transcript part (docs/TRANSCRIPT-SCHEMA.md).
	paneTicker := time.NewTicker(250 * time.Millisecond)
	defer paneTicker.Stop()
	var prompts promptTracker

	sendTyping := func() {
		if err := w.discord.SendTyping(threadID); err != nil {
			log.Printf("mirror: typing: %v", err)
		}
	}
	sendTyping()

	for {
		select {
		case <-ctx.Done():
			if parent.Err() == nil {
				return errMirrorSuperseded
			}
			return ctx.Err()

		case <-typingTicker.C:
			sendTyping()

		case <-livenessTicker.C:
			if tmuxTarget != "" && !isTmuxPaneAlive(tmuxTarget) {
				log.Printf("mirror: tmux pane %s died", tmuxTarget)
				return nil
			}

		case <-paneTicker.C:
			if tmuxTarget == "" {
				continue
			}
			pane, captureErr := capturePaneText(tmuxTarget)
			if captureErr != nil {
				continue
			}
			w.pollPrompts(threadID, tmuxTarget, pane, &prompts)

		case ev, ok := <-ch:
			if !ok {
				if parent.Err() == nil && ctx.Err() != nil {
					return errMirrorSuperseded
				}
				return nil
			}
			w.mirrorEvent(sessionID, threadID, ev, asst, tools, &turn, seenAsst)
		}
	}
}

// mirrorEvent applies one transcript event to the Discord thread.
func (w *Watcher) mirrorEvent(sessionID, threadID string, ev transcript.Event,
	asst map[string]*msgState, tools map[string]*toolState, turn *turnState, seen map[string]bool) {
	switch ev.Kind {
	case transcript.KindUserPrompt:
		*turn = turnState{open: true}
		prompt := ev.Text
		runes := []rune(prompt)
		if len(runes) > 300 {
			prompt = string(runes[:297]) + "..."
		}
		if prompt == "" {
			return
		}
		// Suppress duplicate echo: if injectAndVerify already echoed this
		// prompt (because it had to retry the injection), skip the second
		// Discord post.  Use the raw text for the suppression key (before
		// truncation) so the check is length-invariant.
		if w.suppressedEchoes.suppress(sessionID, ev.Text) {
			log.Printf("mirror: suppressed duplicate echo for session %s", sessionID)
			return
		}
		w.sendToThread(threadID, fmt.Sprintf("**You:** %s", prompt))

	case transcript.KindAssistantText:
		turn.open = true
		key := ev.UUID + "\x00" + ev.Text
		if seen[key] {
			return
		}
		seen[key] = true
		ms := asst[ev.MessageID]
		if ms == nil {
			ms = &msgState{}
			asst[ev.MessageID] = ms
		}
		if ms.text != "" {
			ms.text += "\n\n"
		}
		ms.text += ev.Text
		turn.hadOutput = true
		w.updateTextMessage(threadID, ms)

	case transcript.KindToolCall:
		turn.open = true
		key := ev.ToolID
		if key == "" {
			key = ev.ToolName
		}
		if tools[key] != nil {
			return
		}
		content := formatToolEmbed(ev.ToolName, ev.ToolID, false, false)
		w.throttle.Wait(threadID)
		toolMsg, err := w.discordSend(threadID, content)
		if err != nil {
			log.Printf("mirror: tool embed: %v", err)
			return
		}
		ts := &toolState{callID: ev.ToolID, toolName: ev.ToolName, msgID: toolMsg.ID}
		tools[key] = ts
		turn.tools = append(turn.tools, ts)

	case transcript.KindToolResult:
		ts := tools[ev.ToolID]
		if ts == nil || ts.done || ts.failed {
			return
		}
		if ev.IsError {
			ts.failed = true
		} else {
			ts.done = true
		}
		content := formatToolEmbed(ts.toolName, ts.callID, ts.done, ts.failed)
		w.throttle.Wait(threadID)
		if err := w.discordEdit(threadID, ts.msgID, content); err != nil {
			log.Printf("mirror: update tool embed: %v", err)
		}

	case transcript.KindTurnEnd:
		if !turn.open {
			return // e.g. the turn_duration that follows an interrupt
		}
		if ev.Interrupted {
			w.sendToThread(threadID, "_Turn interrupted_")
		} else {
			if !turn.hadOutput {
				w.sendToThread(threadID, "_Turn finished without output_")
			}
			// Soften failed tool embeds in-place: the turn completed, so flip
			// ❌ failed → ⚠️ warning.
			for _, ts := range turn.tools {
				if ts.failed && ts.msgID != "" {
					w.throttle.Wait(threadID)
					if err := w.discordEdit(threadID, ts.msgID, formatToolEmbedSoftened(ts.toolName, ts.callID)); err != nil {
						log.Printf("mirror: soften tool embed: %v", err)
					}
				}
			}
		}
		w.sendToThread(threadID, fmt.Sprintf("_— %d tool(s)_", len(turn.tools)))
		*turn = turnState{}
	}
}

// sendToThread sends a message to the thread.
func (w *Watcher) sendToThread(threadID, content string) {
	w.throttle.Wait(threadID)
	if _, err := w.discordSend(threadID, content); err != nil {
		log.Printf("mirror: sendToThread: %v", err)
	}
}

// updateTextMessage creates or edits assistant text Discord messages.
//
// Invariant: ms.discordMsgIDs[i] is the Discord message ID for chunk i.
// ms.discordMsgID is the current streaming target (the last ID in discordMsgIDs).
//
// Streaming behavior:
//   - While text fits in one chunk, the same message is edited in place.
//   - When text overflows 2000 chars, the current message is sealed (edited with
//     the final split point) and a new follow-up message is created for the remainder.
//   - This repeats for each additional overflow, so K chunks → K Discord messages.
func (w *Watcher) updateTextMessage(threadID string, ms *msgState) {
	chunks := ChunkText(ms.text, 2000)
	if len(chunks) == 0 {
		return
	}

	N := len(ms.discordMsgIDs) // Discord messages already created
	K := len(chunks)           // chunks needed now

	if N == 0 {
		// First call: create the first Discord message.
		w.throttle.Wait(threadID)
		msg, err := w.discordSend(threadID, chunks[0])
		if err != nil {
			log.Printf("mirror: create text chunk 0: %v", err)
			return
		}
		ms.discordMsgIDs = append(ms.discordMsgIDs, msg.ID)
		ms.discordMsgID = msg.ID
		N = 1
		// Fall through: if K > 1, we need to create more messages below.
	}

	if K > N {
		// We've overflowed into more chunks. Seal all existing messages with their
		// final chunk content, then create messages for any new chunks.

		// Step 1: Seal all existing messages (they may have been written with partial text).
		for i := 0; i < N; i++ {
			w.throttle.Wait(threadID)
			if err := w.discordEdit(threadID, ms.discordMsgIDs[i], chunks[i]); err != nil {
				log.Printf("mirror: seal text chunk %d: %v", i, err)
			}
		}

		// Step 2: Create messages for any remaining chunks.
		for i := N; i < K; i++ {
			w.throttle.Wait(threadID)
			msg, err := w.discordSend(threadID, chunks[i])
			if err != nil {
				log.Printf("mirror: create text chunk %d: %v", i, err)
				return
			}
			ms.discordMsgIDs = append(ms.discordMsgIDs, msg.ID)
			ms.discordMsgID = msg.ID
		}
	} else {
		// Same number of chunks as before (K == N, since text only grows).
		// Just edit the live (last) message with the latest content.
		lastIdx := K - 1
		w.throttle.Wait(threadID)
		if err := w.discordEdit(threadID, ms.discordMsgIDs[lastIdx], chunks[lastIdx]); err != nil {
			log.Printf("mirror: edit text chunk %d: %v", lastIdx, err)
		}
		ms.discordMsgID = ms.discordMsgIDs[lastIdx]
	}
}

// discordSend sends a message with 429 retry logic using the relay lifecycle
// context.  A shutdown can therefore interrupt a RetryAfter wait promptly.
func (w *Watcher) discordSend(channelID, content string) (MsgRef, error) {
	return w.discordSendContext(w.runCtx, channelID, content)
}

func (w *Watcher) discordSendContext(ctx context.Context, channelID, content string) (MsgRef, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	msg, err := w.discord.SendMessage(channelID, content)
	if err == nil {
		return msg, nil
	}
	if retryAfter, ok := extractRetryAfter(err); ok {
		log.Printf("mirror: 429 rate-limit on send, retry after %v", retryAfter)
		w.throttle.SetBackoff(channelID, retryAfter)
		defer w.throttle.ClearBackoff(channelID)
		if err := waitForRetryAfter(ctx, retryAfter); err != nil {
			return MsgRef{}, err
		}
		return w.discord.SendMessage(channelID, content)
	}
	return MsgRef{}, err
}

// discordEdit edits a message with 429 retry logic.
func (w *Watcher) discordEdit(channelID, messageID, content string) error {
	return w.discordEditContext(w.runCtx, channelID, messageID, content)
}

func (w *Watcher) discordEditContext(ctx context.Context, channelID, messageID, content string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	err := w.discord.EditMessage(channelID, messageID, content)
	if err == nil {
		return nil
	}
	if retryAfter, ok := extractRetryAfter(err); ok {
		log.Printf("mirror: 429 rate-limit on edit, retry after %v", retryAfter)
		w.throttle.SetBackoff(channelID, retryAfter)
		defer w.throttle.ClearBackoff(channelID)
		if err := waitForRetryAfter(ctx, retryAfter); err != nil {
			return err
		}
		return w.discord.EditMessage(channelID, messageID, content)
	}
	return err
}

func (w *Watcher) discordSendComponents(channelID, content string, components []Component) (MsgRef, error) {
	// Component writes are ordinary Discord writes: pace them before both the
	// initial request and any 429 retry so controls cannot bypass channel pacing.
	w.throttle.Wait(channelID)
	msg, err := w.discord.SendComponents(channelID, content, components)
	if err == nil {
		return msg, nil
	}
	if retryAfter, ok := extractRetryAfter(err); ok {
		w.throttle.SetBackoff(channelID, retryAfter)
		defer w.throttle.ClearBackoff(channelID)
		ctx := w.runCtx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := waitForRetryAfter(ctx, retryAfter); err != nil {
			return MsgRef{}, err
		}
		w.throttle.Wait(channelID)
		return w.discord.SendComponents(channelID, content, components)
	}
	return MsgRef{}, err
}

func (w *Watcher) discordEditComponents(channelID, messageID, content string, components []Component) error {
	w.throttle.Wait(channelID)
	err := w.discord.EditComponents(channelID, messageID, content, components)
	if err == nil {
		return nil
	}
	if retryAfter, ok := extractRetryAfter(err); ok {
		w.throttle.SetBackoff(channelID, retryAfter)
		defer w.throttle.ClearBackoff(channelID)
		ctx := w.runCtx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := waitForRetryAfter(ctx, retryAfter); err != nil {
			return err
		}
		w.throttle.Wait(channelID)
		return w.discord.EditComponents(channelID, messageID, content, components)
	}
	return err
}

// waitForRetryAfter waits for a relay-owned rate-limit delay without making
// shutdown wait for the complete RetryAfter duration.
func waitForRetryAfter(ctx context.Context, retryAfter time.Duration) error {
	if retryAfter <= 0 {
		return nil
	}
	timer := time.NewTimer(retryAfter)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// extractRetryAfter extracts the retry_after duration from a rate-limit error.
// It checks for:
//  1. *discordgo.RateLimitError (real Discord 429 responses via discordgo)
//  2. retryAfterDurationGetter interface (test doubles)
//  3. String fallback: "retry after <duration>" in error message
func extractRetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}

	// Primary path: discordgo typed rate-limit error.
	var rle *discordgo.RateLimitError
	if errors.As(err, &rle) && rle.RateLimit != nil {
		return rle.RetryAfter, true
	}

	// Secondary path: test double that implements RetryAfterDuration().
	type retryHolder interface {
		RetryAfterDuration() time.Duration
	}
	var rh retryHolder
	if errors.As(err, &rh) {
		return rh.RetryAfterDuration(), true
	}

	// Fallback: parse the error string for "retry after <duration>".
	msg := err.Error()
	const prefix = "retry after "
	if idx := strings.Index(msg, prefix); idx >= 0 {
		rest := msg[idx+len(prefix):]
		// Trim at first space or end.
		end := len(rest)
		for i, c := range rest {
			if c == ' ' || c == ',' {
				end = i
				break
			}
		}
		if d, parseErr := time.ParseDuration(rest[:end]); parseErr == nil {
			return d, true
		}
	}
	return 0, false
}

// formatToolEmbed formats a tool call as a Discord message.
func formatToolEmbed(toolName, callID string, done, failed bool) string {
	status := "⏳ running"
	if done {
		status = "✅ done"
	}
	if failed {
		status = "❌ failed"
	}
	return fmt.Sprintf("🔧 **Tool:** `%s` | %s | ID: `%s`", toolName, status, callID)
}

// formatToolEmbedSoftened formats a previously-failed tool embed as a warning
// (used when the turn completes cleanly, softening the error in-place).
func formatToolEmbedSoftened(toolName, callID string) string {
	return fmt.Sprintf("🔧 **Tool:** `%s` | ⚠️ warning | ID: `%s` | turn completed", toolName, callID)
}

func capturePaneText(target string) (string, error) {
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(tmuxBin, "capture-pane", "-t", target, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// isTmuxPaneAlive checks if a tmux pane target is still alive.
// Uses list-panes -t <target> (window-level check) rather than has-session,
// so a dead pane inside a live session is correctly detected.
func isTmuxPaneAlive(target string) bool {
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return false
	}
	err = exec.Command(tmuxBin, "list-panes", "-t", target).Run()
	return err == nil
}

// promptTracker is mirrorContent's per-session memory of the prompts it posted.
type promptTracker struct {
	approval, question string // signature of the last prompt posted
	approvalMissing    int    // consecutive polls without a permission prompt
	questionMissing    int    // consecutive polls without a question picker
}

// promptAbsentPolls is how many consecutive pane polls must lack a prompt
// before its pending interaction is retired (rides out a torn redraw).
const promptAbsentPolls = 2

// pollPrompts runs one pane poll for approvals and questions. Invariants, shared
// with the trust dialog: a pending interaction is keyed by the exact prompt
// signature, retired when the pane stops showing that prompt or shows a
// different one, and times out (postApproval/postQuestion/askTrust).
func (w *Watcher) pollPrompts(threadID, target, pane string, tr *promptTracker) {
	apShow := inject.IsPermissionPrompt(pane)
	apSig := ""
	if apShow {
		apSig = inject.PermissionSignature(pane)
		tr.approvalMissing = 0
	} else {
		tr.approvalMissing++
	}
	if apShow || tr.approvalMissing >= promptAbsentPolls {
		w.retireInteractions(threadID, target, "approval", apShow, apSig)
	}
	if apShow {
		if !w.pendingInteractions.has(threadID, target, "approval") &&
			(apSig != tr.approval || w.pendingInteractions.settled(threadID, target, "approval", interactionGrace)) {
			if w.postApproval(threadID, target, apSig) {
				tr.approval = apSig
			}
		}
	} else {
		tr.approval = ""
	}

	q, qShow := inject.ParseQuestion(pane)
	qSig := ""
	if qShow {
		qSig = questionSignatureOf(q)
		tr.questionMissing = 0
	} else {
		tr.questionMissing++
	}
	if qShow || tr.questionMissing >= promptAbsentPolls {
		w.retireInteractions(threadID, target, "question", qShow, qSig)
	}
	if qShow {
		if !w.pendingInteractions.has(threadID, target, "question") &&
			(qSig != tr.question || w.pendingInteractions.settled(threadID, target, "question", interactionGrace)) {
			if w.postQuestion(threadID, target, q) {
				tr.question = qSig
			}
		}
	} else {
		tr.question = ""
	}
}

// retireInteractions drops the pending interactions the pane no longer shows
// and marks their Discord messages expired.
func (w *Watcher) retireInteractions(threadID, target, kind string, showing bool, signature string) {
	for _, entry := range w.pendingInteractions.retire(threadID, target, kind, showing, signature) {
		text := "⌛ **Permission request expired** (the prompt is no longer on screen)."
		if kind == "question" {
			text = "⌛ **Question expired** (the picker is no longer on screen)."
		}
		if err := w.discordEditComponents(entry.threadID, entry.messageID, text, nil); err != nil {
			log.Printf("mirror: edit retired %s: %v", kind, err)
		}
	}
}

// postApproval posts the Approve/Deny buttons for a permission prompt. It
// reports whether the message was delivered; callers must only remember the
// prompt signature on success so a failed post is retried on the next poll.
func (w *Watcher) postApproval(threadID, target, signature string) bool {
	stamp := time.Now().UnixNano()
	approveID := fmt.Sprintf("approval:approve:%d", stamp)
	denyID := fmt.Sprintf("approval:deny:%d", stamp)
	content := "⚠️ **Permission required**\nApprove this Claude Code action?"
	if signature != "" {
		detail := signature
		if r := []rune(detail); len(r) > 1200 {
			detail = string(r[:1200]) + "…"
		}
		content += "\n```\n" + strings.ReplaceAll(detail, "```", "'''") + "\n```"
	}
	msg, err := w.discordSendComponents(threadID, content, []Component{
		{Type: "button", CustomID: approveID, Label: "Approve", Style: "success"},
		{Type: "button", CustomID: denyID, Label: "Deny", Style: "danger"},
	})
	if err != nil {
		log.Printf("mirror: post approval: %v", err)
		return false
	}
	entry := pendingInteraction{target: target, threadID: threadID, messageID: msg.ID, kind: "approval", signature: signature}
	w.pendingInteractions.clearResolved(threadID, target, "approval")
	w.pendingInteractions.put(approveID, entry)
	w.pendingInteractions.put(denyID, entry)
	go func() {
		timer := time.NewTimer(w.cfg.ApprovalTimeout)
		defer timer.Stop()
		select {
		case <-w.runCtx.Done():
			return
		case <-timer.C:
			if _, _, ok := w.pendingInteractions.claim(approveID); !ok {
				return // a click or retirement claimed the pair first
			}
			w.pendingInteractions.markResolved(threadID, target, "approval")
			if !w.paneStillShows(entry) {
				_ = w.discordEditComponents(threadID, msg.ID, "⌛ **Permission request expired** (the prompt is no longer on screen).", nil)
				return
			}
			if err := tmuxctl.SendRecipe(target, tmuxctl.ApprovalDenyRecipe); err != nil {
				log.Printf("mirror: approval timeout deny: %v", err)
				return
			}
			if err := w.discordEditComponents(threadID, msg.ID, "⚠️ **Permission denied automatically** (approval timeout).", nil); err != nil {
				log.Printf("mirror: edit timed-out approval: %v", err)
			}
		}
	}()
	return true
}

// postQuestion posts the option picker for an AskUserQuestion prompt and
// reports whether it was delivered.
func (w *Watcher) postQuestion(threadID, target string, q inject.Question) bool {
	id := fmt.Sprintf("question:select:%d", time.Now().UnixNano())
	options := make([]ComponentOption, 0, len(q.Options))
	for i, option := range q.Options {
		options = append(options, ComponentOption{Label: option, Value: fmt.Sprintf("%d", i)})
	}
	msg, err := w.discordSendComponents(threadID, "❓ **Question:** "+q.Prompt, []Component{{
		Type: "select", CustomID: id, Placeholder: "Choose an answer", Options: options,
	}})
	if err != nil {
		log.Printf("mirror: post question picker: %v", err)
		return false
	}
	w.pendingInteractions.clearResolved(threadID, target, "question")
	entry := pendingInteraction{target: target, threadID: threadID, messageID: msg.ID, kind: "question", options: len(q.Options), signature: questionSignatureOf(q)}
	w.pendingInteractions.put(id, entry)
	go func() {
		timer := time.NewTimer(w.cfg.ApprovalTimeout)
		defer timer.Stop()
		select {
		case <-w.runCtx.Done():
		case <-timer.C:
			if _, _, ok := w.pendingInteractions.claim(id); !ok {
				return // answered or retired meanwhile
			}
			w.pendingInteractions.markResolved(threadID, target, "question")
			note := "⌛ **Question expired** (the prompt is no longer on screen)."
			if w.paneStillShows(entry) {
				// Nobody answered: cancel the picker rather than leave the
				// session blocked and the thread's next question suppressed.
				if err := tmuxctl.SendRecipe(target, tmuxctl.QuestionCancelRecipe); err != nil {
					log.Printf("mirror: question timeout cancel: %v", err)
				}
				note = "⌛ **Question timed out** (no answer; the picker was cancelled)."
			}
			if err := w.discordEditComponents(threadID, msg.ID, note, nil); err != nil {
				log.Printf("mirror: edit timed-out question: %v", err)
			}
		}
	}()
	return true
}

func questionSignatureOf(q inject.Question) string {
	return q.Prompt + "\x00" + strings.Join(q.Options, "\x00")
}

// paneStillShows reports whether the pane still shows the very prompt entry was
// posted for. A button must never act on a different (or vanished) prompt.
func (w *Watcher) paneStillShows(entry pendingInteraction) bool {
	capture := w.capturePane
	if capture == nil {
		capture = capturePaneText
	}
	pane, err := capture(entry.target)
	if err != nil {
		return false
	}
	switch entry.kind {
	case "approval":
		return inject.IsPermissionPrompt(pane) && inject.PermissionSignature(pane) == entry.signature
	case "question":
		q, ok := inject.ParseQuestion(pane)
		return ok && questionSignatureOf(q) == entry.signature
	case "trust":
		return inject.IsTrustDialog(pane)
	}
	return false
}

// askTrust surfaces Claude Code's folder-trust dialog to Discord as an approval
// posted in channelID and blocks until it is answered. Approve trusts the
// folder; Deny, a timeout or a shutdown dismisses the dialog (Esc). The relay
// never trusts a folder on its own.
func (w *Watcher) askTrust(ctx context.Context, target, channelID string) bool {
	stamp := time.Now().UnixNano()
	approveID := fmt.Sprintf("trust:approve:%d", stamp)
	denyID := fmt.Sprintf("trust:deny:%d", stamp)
	msg, err := w.discordSendComponents(channelID,
		"🔐 **Trust this folder?**\nClaude Code is asking whether you trust the folder it was started in (`"+target+"`). Approve to trust it and continue; Deny cancels this session.",
		[]Component{
			{Type: "button", CustomID: approveID, Label: "Approve", Style: "success"},
			{Type: "button", CustomID: denyID, Label: "Deny", Style: "danger"},
		})
	if err != nil {
		log.Printf("mirror: post trust approval: %v", err)
		_ = tmuxctl.SendRecipe(target, tmuxctl.TrustDenyRecipe)
		return false
	}
	result := make(chan bool, 1)
	entry := pendingInteraction{target: target, threadID: channelID, messageID: msg.ID, kind: "trust", signature: trustSignature, result: result}
	w.pendingInteractions.put(approveID, entry)
	w.pendingInteractions.put(denyID, entry)

	timeout := time.NewTimer(w.cfg.ApprovalTimeout)
	defer timeout.Stop()
	deny := func(note string) bool {
		if err := tmuxctl.SendRecipe(target, tmuxctl.TrustDenyRecipe); err != nil {
			log.Printf("mirror: trust deny: %v", err)
		}
		if err := w.discordEditComponents(channelID, msg.ID, note, nil); err != nil {
			log.Printf("mirror: edit trust approval: %v", err)
		}
		return false
	}
	// Retire the request when the pane stops showing the dialog (answered in the
	// terminal): the buttons would otherwise act on whatever is on screen next.
	watch := time.NewTicker(trustWatchInterval)
	defer watch.Stop()
	absent := 0
	for {
		select {
		case ok := <-result:
			return ok
		case <-watch.C:
			if w.paneStillShows(entry) {
				absent = 0
				continue
			}
			if absent++; absent < promptAbsentPolls {
				continue
			}
			if _, _, ok := w.pendingInteractions.claim(approveID); !ok {
				return <-result // a click is being handled
			}
			if err := w.discordEditComponents(channelID, msg.ID, "⌛ **Trust request expired** (the dialog is no longer on screen).", nil); err != nil {
				log.Printf("mirror: edit retired trust approval: %v", err)
			}
			// Answered in the terminal: awaitReady keeps polling and sees whether
			// the pane became ready or exited.
			return true
		case <-timeout.C:
			if _, _, ok := w.pendingInteractions.claim(approveID); !ok {
				return <-result // an interaction won the race
			}
			return deny("⛔ **Trust denied automatically** (approval timeout).")
		case <-ctx.Done():
			if _, _, ok := w.pendingInteractions.claim(approveID); !ok {
				return <-result // a click already owns the request
			}
			return deny("⛔ **Trust request cancelled.**")
		}
	}
}

// trustSignature keys the trust dialog's pending interaction: the dialog has
// no variable content, so any pane showing it is "the same prompt".
const trustSignature = "trust-dialog"

// trustWatchInterval is how often askTrust re-checks that the dialog is still up.
const trustWatchInterval = 500 * time.Millisecond

func (w *Watcher) handleInteraction(ev InteractionEvent) {
	if err := w.discord.AcknowledgeInteraction(ev); err != nil {
		log.Printf("mirror: interaction acknowledgement: %v", err)
	}
	if !inject.ShouldAcceptAuthor(ev.AuthorID, ev.IsBot, w.cfg.AllowedUserID) {
		return
	}
	// Claim the whole request (both buttons of a pair) atomically before any
	// pane action: concurrent clicks and the timeout race for it, one wins.
	entry, claimed, ok := w.pendingInteractions.claim(ev.CustomID)
	if !ok || entry.threadID != ev.ChannelID {
		if ok {
			w.pendingInteractions.restore(claimed, entry) // another channel's request
		}
		return
	}
	switch entry.kind {
	case "trust":
		approve := strings.Contains(ev.CustomID, ":approve:")
		// Validate at click time: the dialog may have been answered in the
		// terminal since the last poll. Never send keys into whatever is on screen now.
		if !w.paneStillShows(entry) {
			_ = w.discordEditComponents(entry.threadID, entry.messageID, "⌛ **Trust request expired** (the dialog is no longer on screen).", nil)
			// awaitReady keeps polling and sees whether the pane became ready or exited.
			entry.result <- true
			return
		}
		recipe, note := tmuxctl.TrustDenyRecipe, "⛔ **Trust denied.**"
		if approve {
			recipe, note = tmuxctl.TrustAcceptRecipe, "✅ **Folder trusted.**"
		}
		if err := tmuxctl.SendRecipe(entry.target, recipe); err != nil {
			log.Printf("mirror: trust recipe: %v", err)
			approve = false
		}
		_ = w.discordEditComponents(entry.threadID, entry.messageID, note, nil)
		entry.result <- approve
	case "approval":
		approve := strings.Contains(ev.CustomID, ":approve:")
		recipe, note := tmuxctl.ApprovalDenyRecipe, "⛔ **Permission denied**."
		if approve {
			recipe, note = tmuxctl.ApprovalApproveRecipe, "✅ **Permission approved**."
		}
		// A button only acts on the prompt it was posted for: if the pane moved on
		// (answered locally, or a different action is showing), retire the pair.
		if !w.paneStillShows(entry) {
			w.pendingInteractions.markResolved(entry.threadID, entry.target, "approval")
			_ = w.discordEditComponents(entry.threadID, entry.messageID, "⌛ **Permission request expired** (the prompt changed or was answered elsewhere).", nil)
			return
		}
		if err := tmuxctl.SendRecipe(entry.target, recipe); err != nil {
			log.Printf("mirror: approval recipe: %v", err)
			w.pendingInteractions.restore(claimed, entry) // keep the buttons usable
			return
		}
		w.pendingInteractions.markResolved(entry.threadID, entry.target, "approval")
		_ = w.discordEditComponents(entry.threadID, entry.messageID, note, nil)
	case "question":
		option := -1
		if len(ev.Values) == 1 {
			if n, err := strconv.Atoi(ev.Values[0]); err == nil {
				option = n
			}
		}
		if option < 0 || option >= entry.options {
			w.pendingInteractions.restore(claimed, entry) // malformed click: keep the picker live
			return
		}
		if !w.paneStillShows(entry) {
			w.pendingInteractions.markResolved(entry.threadID, entry.target, "question")
			_ = w.discordEditComponents(entry.threadID, entry.messageID, "⌛ **Question expired** (the picker changed or was answered elsewhere).", nil)
			return
		}
		if err := tmuxctl.SelectQuestion(entry.target, option); err != nil {
			log.Printf("mirror: question recipe: %v", err)
			return
		}
		w.pendingInteractions.markResolved(entry.threadID, entry.target, "question")
		_ = w.discordEditComponents(entry.threadID, entry.messageID, "✅ **Question answered.**", nil)
	}
}

// handleDiscordMessage is registered with the Discord client to receive
// MESSAGE_CREATE gateway events. It dispatches thread replies to the injector.
func (w *Watcher) handleDiscordMessage(ev MessageEvent) {
	// Drop bots and non-allowed authors immediately.
	if !inject.ShouldAcceptAuthor(ev.AuthorID, ev.AuthorIsBot, w.cfg.AllowedUserID) {
		return
	}

	// A message in the relay channel (not a thread) triggers a new session spawn.
	// Check this before checking known threads so the channel message gets spawned
	// even if we somehow also manage a thread with the same ID (impossible in practice,
	// but correctness requires the channel-message path to be checked first).
	if ev.ChannelID == w.cfg.ChannelID {
		log.Printf("mirror: channel message received – spawning new llmux session")
		go func(content string, atts []MessageAttachment) {
			// Use a background context so the spawn survives past handleDiscordMessage.
			if err := w.handleSpawnFromDiscord(context.Background(), content, atts); err != nil {
				log.Printf("mirror: spawn from Discord: %v", err)
			}
		}(ev.Content, ev.Attachments)
		return
	}

	// Find the session that owns this thread.
	var rec *SessionRecord
	for _, r := range w.sessions.All() {
		if r.ThreadID == ev.ChannelID {
			rec = r
			break
		}
	}
	if rec == nil {
		// Not a thread we manage directly, but it may have a pending !resume <n>
		// entry (e.g., from handleAmbiguousResume on a now-ended session).
		// Route through the real handler so the choice is never silently dropped.
		w.handleResumeChoiceForThread(ev.ChannelID, ev.Content)
		return
	}

	log.Printf("mirror: handleDiscordMessage: channel=%s session=%s", ev.ChannelID, rec.SessionID)

	// Check for pending !resume <n> choice first (takes priority).
	if w.handleResumeChoiceForThread(ev.ChannelID, ev.Content) {
		return
	}

	// Handle !quit command: teardown pane but keep the claim.
	if strings.TrimSpace(ev.Content) == "!quit" {
		log.Printf("mirror: !quit received for session %s", rec.SessionID)
		go w.teardownPane(rec.TmuxTarget)
		return
	}

	// Handle bare `!resume` (no number): show candidate list.
	if strings.TrimSpace(ev.Content) == "!resume" {
		go w.handleAmbiguousResume(rec, ev.Content, nil)
		return
	}

	// Handle !title command.
	if title, ok := inject.HandleTitleCommand(ev.Content); ok {
		if err := w.discord.RenameThread(rec.ThreadID, title); err != nil {
			log.Printf("mirror: rename thread %s: %v", rec.ThreadID, err)
		}
		return
	}

	// Build attachment list from image attachments.
	var attachments []inject.Attachment
	for _, att := range ev.Attachments {
		attachments = append(attachments, inject.Attachment{
			Filename: att.Filename,
			URL:      att.URL,
		})
	}

	// If the pane is dead, attempt a resume before injecting.
	if rec.TmuxTarget != "" && !isTmuxPaneAlive(rec.TmuxTarget) {
		log.Printf("mirror: pane %s is dead for session %s – attempting resume", rec.TmuxTarget, rec.SessionID)
		go func(r *SessionRecord, content string, atts []inject.Attachment) {
			if err := w.resumeDeadSession(context.Background(), r, content, atts); err != nil {
				log.Printf("mirror: resumeDeadSession %s: %v", r.SessionID, err)
				w.sendToThread(r.ThreadID, fmt.Sprintf("⚠️ Could not resume session: %v", err))
			}
		}(rec, ev.Content, attachments)
		return
	}

	// Inject in a goroutine so we don't block the gateway event loop.
	go func(target, sessionID, threadID, content string, atts []inject.Attachment) {
		if err := w.injectAndVerify(context.Background(), target, sessionID, threadID, content, atts); err != nil {
			log.Printf("mirror: inject session %s: %v", sessionID, err)
		}
	}(rec.TmuxTarget, rec.SessionID, rec.ThreadID, ev.Content, attachments)
}

// ── Resume dead sessions ───────────────────────────────────────────────────────

// echoVerifyTimeout is how long to wait for the injected prompt to appear in the transcript.
const echoVerifyTimeout = 30 * time.Second

// pendingResumeStore tracks pending !resume <n> candidate lists per Discord thread.
type pendingResumeStore struct {
	mu      sync.Mutex
	entries map[string]*pendingResumeEntry // keyed by threadID
}

type pendingResumeEntry struct {
	candidates []resumeCandidate
	content    string
	atts       []inject.Attachment
	rec        *SessionRecord
}

type resumeCandidate struct {
	SessionID string
	Title     string
	Model     string
	Updated   time.Time
}

func (p *pendingResumeStore) init() {
	p.entries = make(map[string]*pendingResumeEntry)
}

func (p *pendingResumeStore) set(threadID string, e *pendingResumeEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = make(map[string]*pendingResumeEntry)
	}
	p.entries[threadID] = e
}

func (p *pendingResumeStore) get(threadID string) (*pendingResumeEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[threadID]
	return e, ok
}

func (p *pendingResumeStore) delete(threadID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, threadID)
}

// teardownSpawnedSession kills exactly one tmux session, addressed by its exact
// name ("=" disables tmux's prefix/fuzzy matching), so a failed spawn can never
// take down another session's pane. Only relay-spawned "llm-" names are accepted.
func teardownSpawnedSession(sessionName string) {
	if !strings.HasPrefix(sessionName, "llm-") || strings.ContainsAny(sessionName, ":.") {
		log.Printf("mirror: refusing to tear down unexpected session name %q", sessionName)
		return
	}
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return
	}
	log.Printf("mirror: teardown failed spawn session %s", sessionName)
	if out, err := exec.Command(tmuxBin, "kill-session", "-t", "="+sessionName).CombinedOutput(); err != nil {
		log.Printf("mirror: kill-session %s: %v: %s", sessionName, err, strings.TrimSpace(string(out)))
	}
}

// TeardownSpawnedSession is the exported variant used in tests.
func TeardownSpawnedSession(sessionName string) { teardownSpawnedSession(sessionName) }

// teardownPane kills the process group of the tmux pane's process, then kill-window.
// This is the canonical teardown used by !quit and refusal handling.
// The claim in sessions.json is kept.
// Only the window is killed, not the whole session, to be surgical.
func (w *Watcher) teardownPane(tmuxTarget string) {
	teardownPaneTarget(tmuxTarget)
}

// TeardownPane is the exported variant used in tests.
func TeardownPane(tmuxTarget string) {
	teardownPaneTarget(tmuxTarget)
}

// teardownPaneTarget implements the actual teardown logic.
// Sends SIGTERM to the pane's process group, then kills the tmux window only.
func teardownPaneTarget(tmuxTarget string) {
	if tmuxTarget == "" {
		return
	}
	log.Printf("mirror: teardown pane %s", tmuxTarget)

	// Get the PID of the pane's foreground process group leader.
	tmuxBin, tmuxErr := tmuxctl.ResolveTmuxBin()
	if tmuxErr != nil {
		return
	}
	out, err := exec.Command(tmuxBin, "display-message", "-t", tmuxTarget, "-p", "#{pane_pid}").Output()
	if err == nil {
		pid := strings.TrimSpace(string(out))
		if pid != "" {
			// Send SIGTERM to the process group (negative pid).
			exec.Command("kill", "-TERM", "--", "-"+pid).Run() //nolint:errcheck
			time.Sleep(300 * time.Millisecond)
		}
	}

	// Kill only the window (never the whole session) — surgical teardown.
	windowTarget := tmuxTarget
	if idx := strings.LastIndex(tmuxTarget, "."); idx >= 0 {
		windowTarget = tmuxTarget[:idx]
	}
	exec.Command(tmuxBin, "kill-window", "-t", windowTarget).Run() //nolint:errcheck
}

// resumeDeadSession is called when a thread reply arrives for a session whose
// tmux pane is no longer alive. It:
//  1. Spawns llmux --resume <sessionID>.
//  2. Detects refusal: the pane shows "No conversation found with session ID"
//     or dies before it is ready.
//  3. On refusal: teardown, retry once silently.
//  4. On second failure: announces a fresh session was started.
//  5. On success: updates rec.TmuxTarget, re-attaches the transcript mirror and
//     injects the queued message.
func (w *Watcher) resumeDeadSession(ctx context.Context, rec *SessionRecord, content string, atts []inject.Attachment) error {
	log.Printf("mirror: resumeDeadSession: session=%s", rec.SessionID)

	newTarget, refused, err := w.spawnResumeReady(ctx, rec)
	if err != nil {
		return err
	}
	if refused {
		log.Printf("mirror: resume refused for session %s (attempt 1), tearing down and retrying", rec.SessionID)
		w.teardownPane(newTarget)

		// Retry once silently.
		newTarget2, refused2, err2 := w.spawnResumeReady(ctx, rec)
		if err2 != nil || refused2 {
			if refused2 {
				log.Printf("mirror: resume refused for session %s (attempt 2), starting fresh session", rec.SessionID)
				w.teardownPane(newTarget2)
			}
			return w.startFreshSessionAfterRefusal(ctx, rec, content, atts)
		}
		newTarget = newTarget2
	}

	// Resume succeeded: update the session record with the new tmux target.
	rec.TmuxTarget = newTarget
	rec.Status = "active"
	if err := w.sessions.Upsert(rec); err != nil {
		log.Printf("mirror: update sessions.json after resume: %v", err)
	}

	// Re-attach the mirror to the same thread, following only new activity. This
	// supersedes the mirror of the dead pane if it has not noticed the death yet.
	offset := w.store.Size(rec.SessionID)
	go func(sid, threadID, target string) {
		if err := w.mirrorContent(w.runCtx, sid, threadID, target, offset); err != nil && !errors.Is(err, errMirrorSuperseded) {
			log.Printf("mirror: resumed mirror %s: %v", sid, err)
		}
	}(rec.SessionID, rec.ThreadID, newTarget)

	// Inject the queued message into the resumed session.
	return w.injectAndVerify(ctx, newTarget, rec.SessionID, rec.ThreadID, content, atts)
}

// spawnResumeReady launches the resume and waits for the pane. refused reports
// the two refusal signals; any other failure to become ready is an error (the
// pane is torn down).
func (w *Watcher) spawnResumeReady(ctx context.Context, rec *SessionRecord) (target string, refused bool, err error) {
	target, markerAgent, err := w.spawnResume(ctx, rec.SessionID, rec.AgentCmd)
	if err != nil {
		return "", false, fmt.Errorf("spawn resume: %w", err)
	}
	// A resume never erases a stored agent command.
	if rec.AgentCmd == "" && markerAgent != "" {
		rec.AgentCmd = markerAgent
	}
	readyErr := w.awaitReady(ctx, target, rec.ThreadID)
	switch {
	case readyErr == nil:
		return target, false, nil
	case errors.Is(readyErr, errPaneRefused):
		log.Printf("mirror: refusal detected for %s: %v", rec.SessionID, readyErr)
		return target, true, nil
	default:
		w.teardownPane(target)
		return "", false, fmt.Errorf("resume %s: %w", rec.SessionID, readyErr)
	}
}

// llmuxPath resolves the llmux binary.
func (w *Watcher) llmuxPath() (string, error) {
	if w.cfg.LlmuxBin != "" {
		return w.cfg.LlmuxBin, nil
	}
	p, err := exec.LookPath("llmux")
	if err != nil {
		return "", fmt.Errorf("llmux not found on PATH (set LlmuxBin in Config): %w", err)
	}
	return p, nil
}

// llmuxEnv builds the environment for an llmux invocation.
func (w *Watcher) llmuxEnv() []string {
	env := os.Environ()
	if w.cfg.StateDir != "" {
		env = setEnvVar(env, "RELAY_STATE_DIR", w.cfg.StateDir)
	}
	if w.cfg.ProjectsRoot != "" {
		env = setEnvVar(env, "RELAY_CLAUDE_PROJECTS", w.cfg.ProjectsRoot)
	}
	// Every llmux launched by the relay is owned by the launching code path,
	// which binds (or tears down) it itself; the scanner must not claim it.
	env = setEnvVar(env, "RELAY_MARKER_OWNER", markerOwnerSpawn)
	// Unset TMUX so llmux always creates a fresh tmux session (never new-window
	// in the relay's own session if the relay runs inside tmux).
	return setEnvVar(env, "TMUX", "")
}

// runLlmux runs llmux with args and returns the session name it prints, plus
// the parsed pending marker.
func (w *Watcher) runLlmux(ctx context.Context, agentCmd string, args ...string) (sessionName string, marker spawnMarkerJSON, pendingPath string, err error) {
	llmuxBin, err := w.llmuxPath()
	if err != nil {
		return "", marker, "", err
	}
	cmd := exec.CommandContext(ctx, llmuxBin, args...)
	cmd.Env = w.llmuxEnv()
	if agentCmd != "" {
		cmd.Env = setEnvVar(cmd.Env, "RELAY_AGENT_CMD", agentCmd)
	}
	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", marker, "", fmt.Errorf("llmux %s: %w", strings.Join(args, " "), err)
	}
	// llmux prints the session name (llm-<unixtime>-<pid>) to stdout.
	sessionName = strings.TrimSpace(stdoutBuf.String())
	if sessionName == "" {
		return "", marker, "", fmt.Errorf("llmux produced no session name on stdout")
	}
	pendingPath = filepath.Join(w.cfg.StateDir, "pending-"+sessionName)
	data, err := os.ReadFile(pendingPath)
	if err != nil {
		return sessionName, marker, pendingPath, fmt.Errorf("read pending marker for %q: %w", sessionName, err)
	}
	if err := json.Unmarshal(data, &marker); err != nil || marker.Target == "" {
		return sessionName, marker, pendingPath, fmt.Errorf("pending marker for %q is not valid", sessionName)
	}
	return sessionName, marker, pendingPath, nil
}

// spawnResume launches llmux --resume <sessionID> and returns the new tmux
// target and the agent command the marker records. A non-empty agentCmd is the
// session's stored agent and is passed to llmux as RELAY_AGENT_CMD.
func (w *Watcher) spawnResume(ctx context.Context, sessionID, agentCmd string) (string, string, error) {
	if !transcript.ValidSessionID(sessionID) {
		return "", "", fmt.Errorf("refusing to resume malformed session id %q", sessionID)
	}
	log.Printf("mirror: spawnResume: llmux --resume %s", sessionID)
	sessionName, marker, pendingPath, err := w.runLlmux(ctx, agentCmd, "--resume", sessionID)
	if err != nil {
		if sessionName != "" {
			return sessionName + ":0.0", "", nil // marker unreadable; fall back to the default target
		}
		return "", "", err
	}
	// The relay already tracks this session; the marker has served its purpose.
	os.Remove(pendingPath)
	return marker.Target, marker.AgentCmd, nil
}

var (
	// errPaneRefused: the pane showed "No conversation found with session ID",
	// or died before it was ready.
	errPaneRefused = errors.New("pane refused or died before ready")
	errNotReady    = errors.New("pane did not become ready")
	errTrustDenied = errors.New("folder trust was not granted")
)

// awaitReady polls the pane until Claude Code accepts a prompt. A folder-trust
// dialog is surfaced to Discord (notifyChannel) and waited on without counting
// against the readiness timeout. Refusal and a pane that disappears before it
// is ready both return errPaneRefused.
func (w *Watcher) awaitReady(ctx context.Context, target, notifyChannel string) error {
	deadline := time.Now().Add(inject.ReadyTimeout)
	captureErrs := 0
	sawAlive := false
	trustAnswered := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		pane, err := capturePaneText(target)
		if err == nil {
			sawAlive = true
			captureErrs = 0
			switch {
			case inject.IsResumeRefused(pane):
				return fmt.Errorf("%w: %s", errPaneRefused, inject.ResumeRefusedSignature)
			case inject.IsTrustDialog(pane):
				// Ask once. After an approval the dialog can still be on screen
				// for a frame or two; keep waiting for it to go away.
				if !trustAnswered {
					if !w.askTrust(ctx, target, notifyChannel) {
						return errTrustDenied
					}
					trustAnswered = true
					deadline = time.Now().Add(inject.ReadyTimeout)
				}
			case inject.PaneReady(pane):
				return nil
			}
		} else if captureErrs++; captureErrs >= 5 {
			// Observed and then gone, or never observable at all: the process
			// exited (the real CLI exits 1 on a refused resume) before it was
			// ready. Fail closed.
			if sawAlive {
				return fmt.Errorf("%w: pane disappeared before ready", errPaneRefused)
			}
			return fmt.Errorf("%w: pane never observable", errPaneRefused)
		}
		if time.Now().After(deadline) {
			return errNotReady
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// startFreshSessionAfterRefusal starts a brand-new session (no --resume)
// and posts ONE notice to the thread that a fresh session was started.
func (w *Watcher) startFreshSessionAfterRefusal(ctx context.Context, rec *SessionRecord, content string, atts []inject.Attachment) error {
	log.Printf("mirror: starting fresh session after double refusal for %s", rec.SessionID)
	w.sendToThread(rec.ThreadID, "⚠️ Could not resume previous session; a **fresh session** has been started.")

	// Spawn a fresh llmux session (no --resume).
	var msgAtts []MessageAttachment
	for _, a := range atts {
		msgAtts = append(msgAtts, MessageAttachment{Filename: a.Filename, URL: a.URL})
	}
	if err := w.handleSpawnFromDiscord(ctx, content, msgAtts); err != nil {
		return fmt.Errorf("fresh spawn after refusal: %w", err)
	}
	return nil
}

// injectAndVerify injects content into the tmux pane and verifies the echo
// appears in the transcript within echoVerifyTimeout. If the echo is absent, it
// re-sends once and suppresses duplicate echoes in the Discord thread.
func (w *Watcher) injectAndVerify(ctx context.Context, target, sessionID, threadID, content string, atts []inject.Attachment) error {
	// Perform the injection.
	if err := w.injector.Inject(target, sessionID, content, atts); err != nil {
		return fmt.Errorf("inject: %w", err)
	}

	// Verify the echo appears in the transcript within echoVerifyTimeout.
	if w.waitForEcho(ctx, sessionID, content, echoVerifyTimeout) {
		// Echo confirmed — mirrorContent posts the "You: …" line itself.
		return nil
	}

	// Echo absent: re-send once.
	log.Printf("mirror: echo absent for session %s – re-sending once", sessionID)
	// Mark the prompt as already-echoed BEFORE the retry so mirrorContent suppresses
	// the second user-prompt event if the prompt is recorded twice.
	w.suppressedEchoes.add(sessionID, content)
	if err := w.injector.Inject(target, sessionID, content, atts); err != nil {
		return fmt.Errorf("inject retry: %w", err)
	}
	return nil
}

// waitForEcho polls the transcript for the given content as a user prompt of
// the session, returning true if found within timeout.
func (w *Watcher) waitForEcho(ctx context.Context, sessionID, content string, timeout time.Duration) bool {
	if content == "" {
		return true
	}
	needle := content
	if r := []rune(needle); len(r) > 100 {
		needle = string(r[:100])
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if w.store.HasPrompt(sessionID, needle) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// handleAmbiguousResume posts a numbered candidate list of all known sessions
// so the user can pick one with `!resume <n>`.
func (w *Watcher) handleAmbiguousResume(rec *SessionRecord, content string, atts []inject.Attachment) {
	ctx := context.Background()
	var candidates []resumeCandidate
	for _, info := range w.store.List(20) {
		candidates = append(candidates, resumeCandidate{SessionID: info.ID, Title: info.Title, Model: info.Model, Updated: info.Updated})
	}

	if len(candidates) == 0 {
		w.sendToThread(rec.ThreadID, "No sessions available to resume.")
		return
	}

	if len(candidates) == 1 {
		// Only one candidate: resume directly.
		rec2 := *rec
		rec2.SessionID = candidates[0].SessionID
		if err := w.resumeDeadSession(ctx, &rec2, content, atts); err != nil {
			log.Printf("mirror: single candidate resume: %v", err)
			w.sendToThread(rec.ThreadID, fmt.Sprintf("⚠️ Resume failed: %v", err))
		}
		return
	}

	// Build numbered list with title, model and last-active age.
	var sb strings.Builder
	sb.WriteString("**Multiple sessions available — reply `!resume <n>` to choose:**\n")
	for i, c := range candidates {
		model := c.Model
		if model == "" {
			model = "unknown"
		}
		title := c.Title
		if title == "" {
			title = "(untitled)"
		}
		sb.WriteString(fmt.Sprintf("%d. `%s` | %s | model: `%s` | last active: %s\n",
			i+1, c.SessionID, strings.ReplaceAll(title, "`", "'"), model, formatAge(time.Since(c.Updated).Milliseconds())))
	}

	// Split on line boundaries so each message stays within Discord's 2000
	// char limit; entry numbering is already continuous in the full text.
	for _, chunk := range ChunkText(strings.TrimRight(sb.String(), "\n"), 2000) {
		chunk = strings.Trim(chunk, "\n")
		if chunk == "" {
			continue
		}
		w.sendToThread(rec.ThreadID, chunk)
	}

	// Store the pending entry so the next `!resume <n>` resolves it.
	w.pendingResume.set(rec.ThreadID, &pendingResumeEntry{
		candidates: candidates,
		content:    content,
		atts:       atts,
		rec:        rec,
	})
}

// formatAge formats a millisecond duration as a human-readable age string.
func formatAge(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// handleResumeChoiceForThread processes a !resume <n> message in a thread that
// has a pending resume candidate list.
func (w *Watcher) handleResumeChoiceForThread(threadID, content string) bool {
	entry, ok := w.pendingResume.get(threadID)
	if !ok {
		return false
	}
	s := strings.TrimSpace(content)
	if !strings.HasPrefix(s, "!resume ") {
		return false
	}
	nStr := strings.TrimSpace(strings.TrimPrefix(s, "!resume "))
	n := 0
	fmt.Sscanf(nStr, "%d", &n)
	if n < 1 || n > len(entry.candidates) {
		w.sendToThread(threadID, fmt.Sprintf("⚠️ Invalid choice. Please reply `!resume 1`–`!resume %d`.", len(entry.candidates)))
		return true
	}
	chosen := entry.candidates[n-1]
	w.pendingResume.delete(threadID)

	go func() {
		// Re-use the rec from the entry, updating its SessionID to the chosen one.
		rec := entry.rec
		rec.SessionID = chosen.SessionID
		if err := w.resumeDeadSession(context.Background(), rec, entry.content, entry.atts); err != nil {
			log.Printf("mirror: resume choice %s: %v", chosen.SessionID, err)
			w.sendToThread(threadID, fmt.Sprintf("⚠️ Resume failed: %v", err))
		}
	}()
	return true
}

// handleSpawnFromDiscord spawns a new llmux session in response to a Discord
// channel message (outside any thread), waits until Claude Code is ready
// (surfacing a folder-trust dialog to Discord for approval), sends the
// triggering message as the first prompt, waits for the transcript to appear,
// binds the session (via mirrorSession).
//
// Binding contract (docs/BINDING-PROTOCOL.md):
//   - llmux generates the session id and launches `claude --session-id <id>`;
//     the pending marker carries that id. A transcript with exactly this id can
//     only have been written by this launch, so no inference is needed.
//   - Claude Code writes the transcript only when the first prompt is
//     submitted, so the prompt is delivered BEFORE the transcript can exist.
//   - A launch that never becomes ready (dies, refuses, trust denied, times
//     out), or never writes its transcript within the bind timeout, fails
//     closed: the spawned tmux session is torn down, no thread is created and
//     nothing is claimed.
func (w *Watcher) handleSpawnFromDiscord(ctx context.Context, content string, evAtts []MessageAttachment) error {
	spawnStartMS := time.Now().UnixMilli()
	log.Printf("mirror: spawning llmux (spawnStart=%d)", spawnStartMS)

	sessionName, marker, pendingPath, runErr := w.runLlmux(ctx, "")
	if sessionName == "" {
		return fmt.Errorf("llmux run: %w", runErr)
	}
	log.Printf("mirror: llmux spawned session %q", sessionName)

	// From here on any failure tears down ONLY this spawn's own tmux session
	// (addressed by its exact name) and never touches any other pane.
	spawnTarget := sessionName + ":0.0"
	if marker.Target != "" {
		spawnTarget = marker.Target
	}
	spawnFail := func(cause error) error {
		var paneWasAlive bool
		if strings.HasPrefix(spawnTarget, sessionName+":") {
			paneWasAlive = isTmuxPaneAlive("=" + spawnTarget)
			teardownSpawnedSession(sessionName)
		} else {
			// llmux placed the spawn in a window of a host tmux session: kill
			// only that window, never the host session.
			paneWasAlive = isTmuxPaneAlive(spawnTarget)
			teardownPaneTarget(spawnTarget)
		}
		os.Remove(pendingPath)
		if paneWasAlive {
			// The user's message would otherwise vanish silently. No thread
			// exists (nothing is bound), so the notice goes to the channel.
			// A pane that died on its own stays silent (fail closed, no
			// Discord traffic without verified identity).
			w.sendToThread(w.cfg.ChannelID, "⚠️ Could not start a new Claude Code session for your message; the spawned session was shut down. Please try again.")
		}
		return cause
	}
	if runErr != nil {
		return spawnFail(runErr)
	}
	sessionID := marker.SessionID
	if !transcript.ValidSessionID(sessionID) {
		return spawnFail(fmt.Errorf("pending marker for %q carries no valid session id", sessionName))
	}
	tmuxTarget := marker.Target

	// 1. Readiness. A failed probe never binds or mirrors.
	if err := w.awaitReady(ctx, tmuxTarget, w.cfg.ChannelID); err != nil {
		return spawnFail(fmt.Errorf("readiness probe failed for llmux session %q (target=%s); nothing injected, nothing bound: %w", sessionName, tmuxTarget, err))
	}

	// 2. Deliver the triggering message as the first prompt.
	var atts []inject.Attachment
	for _, a := range evAtts {
		atts = append(atts, inject.Attachment{Filename: a.Filename, URL: a.URL})
	}
	if err := w.injector.SendPrompt(tmuxTarget, sessionID, content, atts); err != nil {
		return spawnFail(fmt.Errorf("inject first prompt into llmux session %q: %w", sessionName, err))
	}

	// 3. Only now wait for the transcript (bind timeout starts after the send).
	// The pane must stay alive throughout.
	if err := w.waitForTranscript(ctx, sessionID, tmuxTarget, w.cfg.SpawnBindTimeout); err != nil {
		return spawnFail(fmt.Errorf("session %s of llmux session %q never produced a transcript: %w", sessionID, sessionName, err))
	}

	log.Printf("mirror: bound new session %s to llmux %s (target=%s)", sessionID, sessionName, tmuxTarget)

	// Atomically claim the session.  If scanSessions won the race and already
	// claimed it, mirrorSession is running and nothing else is needed.
	needsMirror := w.claimed.tryAdd(sessionID)

	// Remove the pending marker so scanSessions won't try to bind it again.
	os.Remove(pendingPath)

	if needsMirror {
		// Register the session into sessions.json so that scanSessions' `seen` map
		// picks it up on the next tick and skips it.
		if err := w.sessions.Upsert(&SessionRecord{
			SessionID:  sessionID,
			TmuxTarget: tmuxTarget,
			Status:     "active",
			AgentCmd:   marker.AgentCmd,
		}); err != nil {
			log.Printf("mirror: sessions.json pre-upsert for %s: %v", sessionID, err)
		}

		// Mirror the session concurrently (same as scanSessions does).
		go func(sid, target, agentCmd string) {
			if err := w.mirrorSession(ctx, sid, target, agentCmd); err != nil {
				log.Printf("mirror: mirrorSession(spawn) %s: %v", sid, err)
			}
		}(sessionID, tmuxTarget, marker.AgentCmd)
	}

	return nil
}

// waitForTranscript waits for sessionID's transcript, failing if the pane dies
// or the timeout passes.
func (w *Watcher) waitForTranscript(ctx context.Context, sessionID, tmuxTarget string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		// Fail closed: context, pane liveness and the deadline are all checked
		// on every iteration BEFORE a transcript is accepted, so a transcript
		// that appeared just before the pane died never binds.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if !isTmuxPaneAlive(tmuxTarget) {
			return fmt.Errorf("pane %s died", tmuxTarget)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %v", timeout)
		}
		if _, err := w.store.Path(sessionID); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// setEnvVar sets (or replaces) a KEY=VALUE pair in the given env slice.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
