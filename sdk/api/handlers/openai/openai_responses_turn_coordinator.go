package openai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const codexTurnMetadataHeader = "X-Codex-Turn-Metadata"

const (
	maxResponsesTurnMetadataFieldLength      = 256
	maxResponsesTurnCoordinatorPrincipalKeys = 128
	responsesTurnCoordinatorEntryTTL         = 6 * time.Hour
)

type responsesCodexTurnMetadata struct {
	InstallationID string `json:"installation_id"`
	PromptCacheKey string `json:"prompt_cache_key"`
	SessionID      string `json:"session_id"`
	ThreadID       string `json:"thread_id"`
	TurnID         string `json:"turn_id"`
	WindowID       string `json:"window_id"`
	RequestKind    string `json:"request_kind"`
}

type responsesTurnCoordinationKey struct {
	principalScope string
	installationID string
	promptCacheKey string
	sessionID      string
	turnID         string
}

type responsesTurnCoordinator struct {
	mu             sync.Mutex
	nextGeneration uint64
	turns          map[responsesTurnCoordinationKey]*responsesTurnCoordinationEntry
}

type responsesTurnCoordinationEntry struct {
	main                 *responsesTurnMainStream
	compactionGeneration uint64
	compactionActive     bool
	lastSeen             time.Time
}

type responsesTurnMainStream struct {
	generation                 uint64
	cancel                     context.CancelFunc
	canceledByTurnCoordination *atomic.Bool
}

func newResponsesTurnCoordinator() *responsesTurnCoordinator {
	return &responsesTurnCoordinator{
		turns: make(map[responsesTurnCoordinationKey]*responsesTurnCoordinationEntry),
	}
}

func responsesTurnCoordinationKeyFromContext(c *gin.Context) (responsesTurnCoordinationKey, responsesCodexTurnMetadata, bool) {
	principalScope, okPrincipal := responsesTurnPrincipalScopeFromContext(c)
	if !okPrincipal {
		return responsesTurnCoordinationKey{}, responsesCodexTurnMetadata{}, false
	}
	key, metadata, ok := responsesTurnCoordinationKeyFromRequest(c.Request)
	if !ok {
		return responsesTurnCoordinationKey{}, metadata, false
	}
	key.principalScope = principalScope
	if !key.Valid() {
		return responsesTurnCoordinationKey{}, metadata, false
	}
	return key, metadata, true
}

func responsesTurnCoordinationKeyFromRequest(req *http.Request) (responsesTurnCoordinationKey, responsesCodexTurnMetadata, bool) {
	if req == nil {
		return responsesTurnCoordinationKey{}, responsesCodexTurnMetadata{}, false
	}
	rawMetadata := strings.TrimSpace(req.Header.Get(codexTurnMetadataHeader))
	if rawMetadata == "" {
		return responsesTurnCoordinationKey{}, responsesCodexTurnMetadata{}, false
	}
	var metadata responsesCodexTurnMetadata
	if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
		return responsesTurnCoordinationKey{}, responsesCodexTurnMetadata{}, false
	}
	installationID, okInstallation := firstBoundedNonEmptyString(metadata.InstallationID)
	promptCacheKey, okPromptCache := firstBoundedNonEmptyString(metadata.PromptCacheKey)
	sessionID, okSession := firstBoundedNonEmptyString(metadata.SessionID, metadata.ThreadID)
	turnID, okTurn := responsesTurnIDFromMetadata(metadata, sessionID)
	if !okInstallation || !okPromptCache || !okSession || !okTurn {
		return responsesTurnCoordinationKey{}, metadata, false
	}
	key := responsesTurnCoordinationKey{
		installationID: installationID,
		promptCacheKey: promptCacheKey,
		sessionID:      sessionID,
		turnID:         turnID,
	}
	if key.sessionID == "" || key.turnID == "" {
		return responsesTurnCoordinationKey{}, metadata, false
	}
	return key, metadata, true
}

func responsesTurnMetadataIsCompaction(c *gin.Context) bool {
	_, metadata, ok := responsesTurnCoordinationKeyFromContext(c)
	return ok && metadata.IsCompaction()
}

func (m responsesCodexTurnMetadata) IsMainTurn() bool {
	switch strings.ToLower(strings.TrimSpace(m.RequestKind)) {
	case "", "turn":
		return true
	default:
		return false
	}
}

func (m responsesCodexTurnMetadata) IsCompaction() bool {
	kind := strings.ToLower(strings.TrimSpace(m.RequestKind))
	switch kind {
	case "compact", "compaction", "checkpoint_compaction":
		return true
	default:
		return false
	}
}

func (k responsesTurnCoordinationKey) Valid() bool {
	return k.principalScope != "" && k.sessionID != "" && k.turnID != ""
}

func (c *responsesTurnCoordinator) BeginMain(ctx context.Context, key responsesTurnCoordinationKey) (context.Context, func(), func() bool) {
	if c == nil || !key.Valid() {
		return ctx, func() {}, func() bool { return false }
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mainCtx, cancelMain := context.WithCancel(ctx)
	canceledByTurnCoordination := &atomic.Bool{}
	var cancelPrevious context.CancelFunc
	var cancelPreviousByTurnCoordination *atomic.Bool
	cancelCurrent := false
	var generation uint64
	now := time.Now()

	c.mu.Lock()
	c.cleanupExpiredLocked(now)
	entry, okEntry := c.entryLocked(key, now)
	if !okEntry {
		c.mu.Unlock()
		return mainCtx, cancelMain, canceledByTurnCoordination.Load
	}
	c.nextGeneration++
	generation = c.nextGeneration
	if entry.main != nil {
		cancelPrevious = entry.main.cancel
		cancelPreviousByTurnCoordination = entry.main.canceledByTurnCoordination
	}
	if entry.compactionActive {
		cancelCurrent = true
	} else {
		entry.main = &responsesTurnMainStream{
			generation:                 generation,
			cancel:                     cancelMain,
			canceledByTurnCoordination: canceledByTurnCoordination,
		}
	}
	c.mu.Unlock()

	if cancelPrevious != nil {
		if cancelPreviousByTurnCoordination != nil {
			cancelPreviousByTurnCoordination.Store(true)
		}
		cancelPrevious()
	}
	if cancelCurrent {
		canceledByTurnCoordination.Store(true)
		cancelMain()
	}

	return mainCtx, func() {
		c.finishMain(key, generation, cancelMain)
	}, canceledByTurnCoordination.Load
}

func (c *responsesTurnCoordinator) BeginCompaction(key responsesTurnCoordinationKey) func() {
	if c == nil || !key.Valid() {
		return func() {}
	}
	var cancelMain context.CancelFunc
	var canceledByTurnCoordination *atomic.Bool
	var generation uint64
	now := time.Now()

	c.mu.Lock()
	c.cleanupExpiredLocked(now)
	entry, okEntry := c.entryLocked(key, now)
	if !okEntry {
		c.mu.Unlock()
		return func() {}
	}
	c.nextGeneration++
	generation = c.nextGeneration
	if entry.main != nil {
		cancelMain = entry.main.cancel
		canceledByTurnCoordination = entry.main.canceledByTurnCoordination
		entry.main = nil
	}
	entry.compactionGeneration = generation
	entry.compactionActive = true
	c.mu.Unlock()

	if cancelMain != nil {
		if canceledByTurnCoordination != nil {
			canceledByTurnCoordination.Store(true)
		}
		cancelMain()
	}

	return func() {
		c.finishCompaction(key, generation)
	}
}

func (c *responsesTurnCoordinator) entryLocked(key responsesTurnCoordinationKey, now time.Time) (*responsesTurnCoordinationEntry, bool) {
	entry := c.turns[key]
	if entry != nil {
		entry.lastSeen = now
		return entry, true
	}
	if c.principalKeyCountLocked(key.principalScope) >= maxResponsesTurnCoordinatorPrincipalKeys {
		return nil, false
	}
	entry = &responsesTurnCoordinationEntry{lastSeen: now}
	c.turns[key] = entry
	return entry, true
}

func (c *responsesTurnCoordinator) finishMain(key responsesTurnCoordinationKey, generation uint64, cancel context.CancelFunc) {
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.turns[key]
	if entry == nil {
		return
	}
	if entry.main != nil && entry.main.generation == generation {
		entry.main = nil
	}
	c.cleanupLocked(key, entry)
}

func (c *responsesTurnCoordinator) finishCompaction(key responsesTurnCoordinationKey, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.turns[key]
	if entry == nil {
		return
	}
	if entry.compactionActive && entry.compactionGeneration == generation {
		entry.compactionActive = false
	}
	c.cleanupLocked(key, entry)
}

func (c *responsesTurnCoordinator) cleanupLocked(key responsesTurnCoordinationKey, entry *responsesTurnCoordinationEntry) {
	if entry.main == nil && !entry.compactionActive {
		delete(c.turns, key)
	}
}

func (c *responsesTurnCoordinator) cleanupExpiredLocked(now time.Time) {
	for key, entry := range c.turns {
		if entry == nil || (!entry.lastSeen.IsZero() && now.Sub(entry.lastSeen) > responsesTurnCoordinatorEntryTTL) {
			delete(c.turns, key)
		}
	}
}

func (c *responsesTurnCoordinator) principalKeyCountLocked(principalScope string) int {
	count := 0
	for key := range c.turns {
		if key.principalScope == principalScope {
			count++
		}
	}
	return count
}

func firstBoundedNonEmptyString(values ...string) (string, bool) {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			if len(trimmed) > maxResponsesTurnMetadataFieldLength {
				return "", false
			}
			return trimmed, true
		}
	}
	return "", true
}

func responsesTurnIDFromMetadata(metadata responsesCodexTurnMetadata, sessionID string) (string, bool) {
	if turnID, ok := firstBoundedNonEmptyString(metadata.TurnID); !ok || turnID != "" {
		return turnID, ok
	}
	windowID, ok := firstBoundedNonEmptyString(metadata.WindowID)
	if !ok || windowID == "" {
		return "", ok
	}
	if !responsesTurnWindowIDLooksTurnScoped(windowID, sessionID) {
		return "", false
	}
	return windowID, true
}

func responsesTurnWindowIDLooksTurnScoped(windowID, sessionID string) bool {
	windowID = strings.TrimSpace(windowID)
	sessionID = strings.TrimSpace(sessionID)
	if windowID == "" || windowID == sessionID {
		return false
	}
	return strings.ContainsAny(windowID, ":/#")
}

func responsesTurnPrincipalScopeFromContext(c *gin.Context) (string, bool) {
	if c == nil {
		return "", false
	}
	provider, okProvider := boundedCoordinationContextString(c, "accessProvider")
	if !okProvider || provider == "" {
		return "", false
	}
	principal := strings.TrimSpace(coordinationContextString(c, "userApiKey"))
	if principal == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(provider + "\x00" + principal))
	return provider + ":" + hex.EncodeToString(sum[:16]), true
}

func boundedCoordinationContextString(c *gin.Context, key string) (string, bool) {
	value := strings.TrimSpace(coordinationContextString(c, key))
	if value == "" {
		return "", true
	}
	if len(value) > maxResponsesTurnMetadataFieldLength {
		return "", false
	}
	return value, true
}

func coordinationContextString(c *gin.Context, key string) string {
	if c == nil {
		return ""
	}
	value, ok := c.Get(key)
	if !ok || value == nil {
		return ""
	}
	if str, okString := value.(string); okString {
		return str
	}
	return fmt.Sprint(value)
}

func (h *OpenAIResponsesAPIHandler) beginResponsesMainTurn(ctx context.Context, key responsesTurnCoordinationKey) (context.Context, func(), func() bool) {
	if h == nil || h.responsesTurnCoordinator == nil {
		return ctx, func() {}, func() bool { return false }
	}
	return h.responsesTurnCoordinator.BeginMain(ctx, key)
}

func (h *OpenAIResponsesAPIHandler) beginResponsesTurnCompaction(c *gin.Context) func() {
	if h == nil || h.responsesTurnCoordinator == nil {
		return func() {}
	}
	key, _, ok := responsesTurnCoordinationKeyFromContext(c)
	if !ok {
		return func() {}
	}
	return h.responsesTurnCoordinator.BeginCompaction(key)
}
