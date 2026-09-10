package limiter

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type clientBucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
	mu         sync.Mutex
}

// Limiter implements a thread-safe in-memory token bucket rate limiter.
type Limiter struct {
	rate     float64 // tokens added per second
	burst    int     // maximum burst capacity
	ttl      time.Duration
	clients  map[string]*clientBucket
	mu       sync.RWMutex
	stopChan chan struct{}
}

// New creates a new Limiter with the given rate (tokens/sec), burst capacity,
// periodic cleanup interval, and idle client TTL.
func New(rate float64, burst int, cleanupInterval, ttl time.Duration) *Limiter {
	l := &Limiter{
		rate:     rate,
		burst:    burst,
		ttl:      ttl,
		clients:  make(map[string]*clientBucket),
		stopChan: make(chan struct{}),
	}

	if cleanupInterval > 0 {
		go l.startCleanup(cleanupInterval)
	}

	return l
}

// Stop terminates the background cleanup goroutine.
func (l *Limiter) Stop() {
	select {
	case <-l.stopChan:
		// already closed
	default:
		close(l.stopChan)
	}
}

func (l *Limiter) startCleanup(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for {
		select {
		case <-ticker.C:
			l.Cleanup()
		case <-l.stopChan:
			ticker.Stop()
			return
		}
	}
}

// Cleanup removes client buckets that have not been seen for longer than ttl.
func (l *Limiter) Cleanup() {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	for key, bucket := range l.clients {
		bucket.mu.Lock()
		idle := now.Sub(bucket.lastSeen) > l.ttl
		bucket.mu.Unlock()

		if idle {
			delete(l.clients, key)
		}
	}
}

// Allow reports whether a request for the given key is permitted.
// If rejected, returns false and the duration until at least one token becomes available.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	bucket := l.getBucket(key)

	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(bucket.lastRefill).Seconds()
	bucket.lastRefill = now
	bucket.lastSeen = now

	// Refill tokens
	bucket.tokens += elapsed * l.rate
	if bucket.tokens > float64(l.burst) {
		bucket.tokens = float64(l.burst)
	}

	// Consume 1 token if available
	if bucket.tokens >= 1.0 {
		bucket.tokens -= 1.0
		return true, 0
	}

	// Calculate wait time for the next token
	needed := 1.0 - bucket.tokens
	retrySeconds := math.Ceil(needed / l.rate)
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	return false, time.Duration(retrySeconds) * time.Second
}

func (l *Limiter) getBucket(key string) *clientBucket {
	l.mu.RLock()
	bucket, exists := l.clients[key]
	l.mu.RUnlock()

	if exists {
		return bucket
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Double check after acquiring write lock
	if bucket, exists = l.clients[key]; exists {
		return bucket
	}

	now := time.Now()
	bucket = &clientBucket{
		tokens:     float64(l.burst),
		lastRefill: now,
		lastSeen:   now,
	}
	l.clients[key] = bucket
	return bucket
}

// Count returns the current number of tracked clients.
func (l *Limiter) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.clients)
}

// KeyExtractor extracts an identifier string from the request context.
type KeyExtractor func(c *gin.Context) string

// IPKeyExtractor extracts client IP.
func IPKeyExtractor(c *gin.Context) string {
	return "ip:" + c.ClientIP()
}

// UserOrIPKeyExtractor extracts userID or token if authenticated, otherwise client IP.
func UserOrIPKeyExtractor(c *gin.Context) string {
	if v, exists := c.Get("userID"); exists {
		if uid, ok := v.(string); ok && uid != "" {
			return "user:" + uid
		}
	}
	h := c.GetHeader("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if tok != "" {
			return "tok:" + tok
		}
	}
	if q := strings.TrimSpace(c.Query("token")); q != "" {
		return "tok:" + q
	}
	return "ip:" + c.ClientIP()
}

// Middleware creates a Gin middleware that enforces rate limiting per key.
func (l *Limiter) Middleware(extractor KeyExtractor) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := extractor(c)
		allowed, retryAfter := l.Allow(key)
		if !allowed {
			retrySeconds := int(math.Ceil(retryAfter.Seconds()))
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			c.Header("Retry-After", fmt.Sprintf("%d", retrySeconds))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "RATE_LIMIT_EXCEEDED",
					"message": "Too many requests. Please try again later.",
				},
			})
			return
		}
		c.Next()
	}
}
