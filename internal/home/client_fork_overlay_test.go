package home

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The historical fork test name is retained for symbol-survival checks. Home
// now exposes compare-and-swap as a native CAS command rather than EVAL.
func TestKVCompareAndSwapReturnsScriptResult(t *testing.T) {
	client, commands := newRedisCommandTestClient(t, func(args []string) string {
		if len(args) > 0 && strings.EqualFold(args[0], "CAS") {
			return ":1\r\n"
		}
		return "-ERR unexpected command\r\n"
	})

	swapped, errCAS := client.KVCompareAndSwap(context.Background(), "key", []byte("old"), true, []byte("new"), 1500*time.Millisecond)
	if errCAS != nil {
		t.Fatalf("KVCompareAndSwap() error = %v", errCAS)
	}
	if !swapped {
		t.Fatal("KVCompareAndSwap() swapped = false, want true")
	}
	if lastCommand := commands.Last(); len(lastCommand) < 2 || !strings.EqualFold(lastCommand[0], "CAS") {
		t.Fatalf("last command = %#v, want CAS", lastCommand)
	}
}

func TestRunConfigSubscriberLifetimeDoesNotReadyWhenFreshCommandProbeFails(t *testing.T) {
	configPayload := "host: 127.0.0.1\n"
	client, _ := newRedisCommandTestClient(t, func(args []string) string {
		switch {
		case len(args) >= 1 && strings.EqualFold(args[0], "HELLO"):
			return "%6\r\n$6\r\nserver\r\n$5\r\nredis\r\n$5\r\nproto\r\n:3\r\n$2\r\nid\r\n:1\r\n$4\r\nmode\r\n$10\r\nstandalone\r\n$4\r\nrole\r\n$6\r\nmaster\r\n$7\r\nmodules\r\n*0\r\n"
		case len(args) >= 2 && strings.EqualFold(args[0], "GET") && args[1] == redisKeyConfig:
			return fmt.Sprintf("$%d\r\n%s\r\n", len(configPayload), configPayload)
		case len(args) >= 2 && strings.EqualFold(args[0], "SUBSCRIBE") && args[1] == redisChannelConfig:
			return "*3\r\n$9\r\nsubscribe\r\n$6\r\nconfig\r\n:1\r\n"
		case len(args) >= 1 && strings.EqualFold(args[0], "PING"):
			return "-ERR fresh command probe failed\r\n"
		default:
			return "+OK\r\n"
		}
	})
	ready := make(chan struct{}, 1)
	errRun := client.RunConfigSubscriberLifetime(context.Background(), func([]byte) error { return nil }, func() { ready <- struct{}{} })
	if errRun == nil {
		t.Fatal("RunConfigSubscriberLifetime() error = nil, want fresh command probe failure")
	}
	select {
	case <-ready:
		t.Fatalf("RunConfigSubscriberLifetime() invoked onReady after fresh command probe failure: %v", errRun)
	default:
	}
	client.mu.Lock()
	commandClient, subscriptionClient := client.cmd, client.sub
	client.mu.Unlock()
	if commandClient != nil || subscriptionClient != nil {
		t.Fatalf("clients retained after fresh command probe failure: command=%v subscription=%v", commandClient != nil, subscriptionClient != nil)
	}
}
