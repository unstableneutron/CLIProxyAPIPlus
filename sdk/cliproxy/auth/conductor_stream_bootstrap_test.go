package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestReadStreamBootstrapTreatsBootstrapMarkerAsStarted(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk, 2)
	ch <- cliproxyexecutor.StreamChunk{Bootstrap: true}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("first visible payload")}

	buffered, closed, bootstrapped, err := readStreamBootstrap(context.Background(), ch)
	if err != nil {
		t.Fatalf("readStreamBootstrap() error = %v", err)
	}
	if closed {
		t.Fatal("readStreamBootstrap() closed = true, want false after bootstrap marker")
	}
	if !bootstrapped {
		t.Fatal("readStreamBootstrap() bootstrapped = false, want true after bootstrap marker")
	}
	if len(buffered) != 0 {
		t.Fatalf("buffered chunks = %d, want bootstrap marker to be consumed without forwarding", len(buffered))
	}

	select {
	case chunk := <-ch:
		if string(chunk.Payload) != "first visible payload" {
			t.Fatalf("next chunk payload = %q, want first visible payload", chunk.Payload)
		}
	default:
		t.Fatal("visible payload was consumed while handling bootstrap marker")
	}
}
