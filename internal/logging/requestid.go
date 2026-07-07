package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// requestIDKey is the context key for storing/retrieving request IDs.
type requestIDKey struct{}

// ginRequestIDKey is the Gin context key for request IDs.
const ginRequestIDKey = "__request_id__"
const ginRequestEventLoggerKey = "__request_event_logger__"
const ginRequestEventSequenceKey = "__request_event_sequence__"

// GenerateRequestID creates a new 8-character hex request ID.
func GenerateRequestID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// WithRequestID returns a new context with the request ID attached.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// GetRequestID retrieves the request ID from the context.
// Returns empty string if not found.
func GetRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// SetGinRequestID stores the request ID in the Gin context.
func SetGinRequestID(c *gin.Context, requestID string) {
	if c != nil {
		c.Set(ginRequestIDKey, requestID)
	}
}

// GetGinRequestID retrieves the request ID from the Gin context.
func GetGinRequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if id, exists := c.Get(ginRequestIDKey); exists {
		if s, ok := id.(string); ok {
			return s
		}
	}
	return ""
}

// SetGinRequestEventLogger stores the optional JSONL request event logger in Gin context.
func SetGinRequestEventLogger(c *gin.Context, logger *AsyncRequestEventLogger) {
	if c == nil || logger == nil || !logger.IsEnabled() {
		return
	}
	c.Set(ginRequestEventLoggerKey, logger)
	c.Set(ginRequestEventSequenceKey, &atomic.Uint64{})
}

// GetGinRequestEventLogger retrieves the optional JSONL request event logger from Gin context.
func GetGinRequestEventLogger(c *gin.Context) *AsyncRequestEventLogger {
	if c == nil {
		return nil
	}
	value, exists := c.Get(ginRequestEventLoggerKey)
	if !exists {
		return nil
	}
	logger, ok := value.(*AsyncRequestEventLogger)
	if !ok || logger == nil || !logger.IsEnabled() {
		return nil
	}
	return logger
}

// NextGinRequestEventSequence returns the next per-request event sequence number.
func NextGinRequestEventSequence(c *gin.Context) uint64 {
	if c == nil {
		return 0
	}
	value, exists := c.Get(ginRequestEventSequenceKey)
	if !exists {
		seq := &atomic.Uint64{}
		c.Set(ginRequestEventSequenceKey, seq)
		return seq.Add(1)
	}
	seq, ok := value.(*atomic.Uint64)
	if !ok || seq == nil {
		return 0
	}
	return seq.Add(1)
}
