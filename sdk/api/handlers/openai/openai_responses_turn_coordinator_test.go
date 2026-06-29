package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestResponsesTurnCoordinatorCancelsOnlySameTurnMain(t *testing.T) {
	coordinator := newResponsesTurnCoordinator()
	keyA := responsesTurnCoordinationKey{principalScope: "principal-1", sessionID: "session-1", turnID: "turn-1"}
	keyB := responsesTurnCoordinationKey{principalScope: "principal-1", sessionID: "session-1", turnID: "turn-2"}

	ctxA, finishA, canceledA := coordinator.BeginMain(context.Background(), keyA)
	defer finishA()
	ctxB, finishB, canceledB := coordinator.BeginMain(context.Background(), keyB)
	defer finishB()

	finishCompaction := coordinator.BeginCompaction(keyB)
	defer finishCompaction()

	if err := ctxA.Err(); err != nil {
		t.Fatalf("different turn context was canceled: %v", err)
	}
	if err := ctxB.Err(); err == nil {
		t.Fatal("same turn context was not canceled")
	}
	if canceledA() {
		t.Fatal("different turn was marked canceled by coordination")
	}
	if !canceledB() {
		t.Fatal("same turn was not marked canceled by coordination")
	}
}

func TestResponsesTurnCoordinatorSeparatesInstallations(t *testing.T) {
	coordinator := newResponsesTurnCoordinator()
	keyA := responsesTurnCoordinationKey{principalScope: "principal-1", installationID: "install-1", sessionID: "session-1", turnID: "turn-1"}
	keyB := responsesTurnCoordinationKey{principalScope: "principal-1", installationID: "install-2", sessionID: "session-1", turnID: "turn-1"}

	ctxA, finishA, _ := coordinator.BeginMain(context.Background(), keyA)
	defer finishA()
	ctxB, finishB, _ := coordinator.BeginMain(context.Background(), keyB)
	defer finishB()

	finishCompaction := coordinator.BeginCompaction(keyB)
	defer finishCompaction()

	if err := ctxA.Err(); err != nil {
		t.Fatalf("different installation context was canceled: %v", err)
	}
	if err := ctxB.Err(); err == nil {
		t.Fatal("same installation context was not canceled")
	}
}

func TestResponsesTurnCoordinatorRejectsMainWhileCompactionActive(t *testing.T) {
	coordinator := newResponsesTurnCoordinator()
	key := responsesTurnCoordinationKey{principalScope: "principal-1", sessionID: "session-1", turnID: "turn-1"}

	finishCompaction := coordinator.BeginCompaction(key)
	ctxDuringCompaction, finishDuringCompaction, canceledDuringCompaction := coordinator.BeginMain(context.Background(), key)
	defer finishDuringCompaction()
	if err := ctxDuringCompaction.Err(); err == nil {
		t.Fatal("main turn context was not canceled while compaction was active")
	}
	if !canceledDuringCompaction() {
		t.Fatal("main turn was not marked canceled while compaction was active")
	}

	finishCompaction()
	ctxAfterCompaction, finishAfterCompaction, _ := coordinator.BeginMain(context.Background(), key)
	defer finishAfterCompaction()
	if err := ctxAfterCompaction.Err(); err != nil {
		t.Fatalf("main turn context after compaction was canceled: %v", err)
	}
}

func TestResponsesTurnCoordinatorMarksReplacedMainAsCoordinated(t *testing.T) {
	coordinator := newResponsesTurnCoordinator()
	key := responsesTurnCoordinationKey{principalScope: "principal-1", sessionID: "session-1", turnID: "turn-1"}

	ctxFirst, finishFirst, canceledFirst := coordinator.BeginMain(context.Background(), key)
	defer finishFirst()
	ctxSecond, finishSecond, canceledSecond := coordinator.BeginMain(context.Background(), key)
	defer finishSecond()

	if err := ctxFirst.Err(); err == nil {
		t.Fatal("first main turn context was not canceled by replacement")
	}
	if !canceledFirst() {
		t.Fatal("first main turn was not marked canceled by coordination")
	}
	if err := ctxSecond.Err(); err != nil {
		t.Fatalf("second main turn context was canceled: %v", err)
	}
	if canceledSecond() {
		t.Fatal("second main turn was unexpectedly marked canceled by coordination")
	}
}

func TestResponsesTurnCoordinatorSeparatesPrincipals(t *testing.T) {
	coordinator := newResponsesTurnCoordinator()
	keyA := responsesTurnCoordinationKey{principalScope: "principal-1", sessionID: "session-1", turnID: "turn-1"}
	keyB := responsesTurnCoordinationKey{principalScope: "principal-2", sessionID: "session-1", turnID: "turn-1"}

	ctxA, finishA, _ := coordinator.BeginMain(context.Background(), keyA)
	defer finishA()
	ctxB, finishB, _ := coordinator.BeginMain(context.Background(), keyB)
	defer finishB()

	finishCompaction := coordinator.BeginCompaction(keyB)
	defer finishCompaction()

	if err := ctxA.Err(); err != nil {
		t.Fatalf("different principal context was canceled: %v", err)
	}
	if err := ctxB.Err(); err == nil {
		t.Fatal("same principal context was not canceled")
	}
}

func TestResponsesTurnCoordinationKeyFromContextUsesScopedWindowFallback(t *testing.T) {
	c := testResponsesTurnCoordinationContext(t, `{"installation_id":"install-1","prompt_cache_key":"cache-1","session_id":"session-1","window_id":"session-1:2","request_kind":"turn"}`, "access", "secret-key")

	key, metadata, ok := responsesTurnCoordinationKeyFromContext(c)

	if !ok {
		t.Fatal("expected turn metadata to produce a coordination key")
	}
	if key.principalScope == "" || strings.Contains(key.principalScope, "secret-key") {
		t.Fatalf("coordination key principal scope = %q, want hashed principal scope", key.principalScope)
	}
	if key.sessionID != "session-1" || key.turnID != "session-1:2" {
		t.Fatalf("coordination key = %#v, want session/window fallback", key)
	}
	if key.installationID != "install-1" || key.promptCacheKey != "cache-1" {
		t.Fatalf("coordination key = %#v, want installation and prompt cache identity", key)
	}
	if !metadata.IsMainTurn() {
		t.Fatal("turn metadata should be treated as a main turn")
	}
}

func TestResponsesTurnCoordinationKeyFromContextRequiresPrincipal(t *testing.T) {
	c := testResponsesTurnCoordinationContext(t, `{"session_id":"session-1","turn_id":"turn-1","request_kind":"turn"}`, "", "")

	if key, _, ok := responsesTurnCoordinationKeyFromContext(c); ok {
		t.Fatalf("coordination key = %#v, want no key without server-side principal", key)
	}
}

func TestResponsesTurnCoordinationKeyRejectsBroadWindowFallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set(codexTurnMetadataHeader, `{"session_id":"session-1","window_id":"session-1","request_kind":"turn"}`)

	if key, _, ok := responsesTurnCoordinationKeyFromRequest(req); ok {
		t.Fatalf("coordination key = %#v, want unscoped window id rejected", key)
	}
}

func TestResponsesTurnCoordinationKeyRejectsOversizedFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set(codexTurnMetadataHeader, `{"session_id":"session-1","turn_id":"`+strings.Repeat("x", maxResponsesTurnMetadataFieldLength+1)+`","request_kind":"turn"}`)

	if key, _, ok := responsesTurnCoordinationKeyFromRequest(req); ok {
		t.Fatalf("coordination key = %#v, want oversized metadata rejected", key)
	}
}

func TestResponsesTurnMetadataRecognizesCompactRequestKinds(t *testing.T) {
	for _, kind := range []string{"compact", "compaction", "checkpoint_compaction"} {
		metadata := responsesCodexTurnMetadata{RequestKind: kind}
		if !metadata.IsCompaction() {
			t.Fatalf("request kind %q should be treated as compaction", kind)
		}
	}
}

func TestResponsesTurnMetadataRejectsBroadCompactRequestKinds(t *testing.T) {
	for _, kind := range []string{"not_compaction", "post_compaction_probe", "compactionish"} {
		metadata := responsesCodexTurnMetadata{RequestKind: kind}
		if metadata.IsCompaction() {
			t.Fatalf("request kind %q should not be treated as compaction", kind)
		}
	}
}

func testResponsesTurnCoordinationContext(t *testing.T, metadata string, provider string, principal string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set(codexTurnMetadataHeader, metadata)
	if provider != "" {
		c.Set("accessProvider", provider)
	}
	if principal != "" {
		c.Set("userApiKey", principal)
	}
	return c
}
