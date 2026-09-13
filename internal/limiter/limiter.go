package limiter

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"squadup/backend/internal/redisx"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type clientBucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
	mu         sync.Mutex
}

var luaTokenBucket = redis.NewScript(`
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local data = redis.call("HMGET", key, "tokens", "last_refill")
local tokens = tonumber(data[1])
local last_refill = tonumber(data[2])

if not tokens then
    tokens = capacity
    last_refill = now
else
    local elapsed = now - last_refill
    tokens = math.min(capacity, tokens + (elapsed * rate))
    last_refill = now
end

if tokens >= cost then
    tokens = tokens - cost
    redis.call("HMSET", key, "tokens", tokens, "last_refill", last_refill)
    local ttl = math.ceil((capacity - tokens) / rate) + 60
    if ttl < 60 then ttl = 60 end
    redis.call("EXPIRE", key, ttl)
    return {1, 0}
else
    redis.call("HMSET", key, "tokens", tokens, "last_refill", last_refill)
    local wait = math.ceil((cost - tokens) / rate)
    if wait < 1 then wait = 1 end
    return {0, wait}
end
`)

// Limiter implements a thread-safe rate limiter with Redis distributed backing
// and in-memory token bucket fallback.
type Limiter struct {
	rate        float64 // tokens added per second
	burst       int     // maximum burst capacity
	ttl         time.Duration
	clients     map[string]*clientBucket
	mu          sync.RWMutex
	stopChan    chan struct{}
	redisClient *redisx.Client
}

// New creates a new Limiter with the given rate (tokens/sec), burst capacity,
// periodic cleanup interval, and idle client TTL.
func New(rate float64, burst int, cleanupInterval, ttl time.Duration) *Limiter {
	return NewDistributed(rate, burst, cleanupInterval, ttl, nil)
}

// NewDistributed creates a Limiter with an optional Redis client.
func NewDistributed(rate float64, burst int, cleanupInterval, ttl time.Duration, rc *redisx.Client) *Limiter {
	l := &Limiter{
		rate:        rate,
		burst:       burst,
		ttl:         ttl,
		clients:     make(map[string]*clientBucket),
		stopChan:    make(chan struct{}),
		redisClient: rc,
	}

	if cleanupInterval > 0 {
		go l.startCleanup(cleanupInterval)
	}

	return l
}

// AttachRedis binds a Redis client to an existing Limiter.
func (l *Limiter) AttachRedis(rc *redisx.Client) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.redisClient = rc
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
	l.mu.RLock()
	rc := l.redisClient
	l.mu.RUnlock()

	if rc != nil && rc.IsAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		nowSec := float64(time.Now().UnixNano()) / 1e9
		res, err := luaTokenBucket.Run(
			ctx,
			rc.Raw(),
			[]string{"squadup:rl:" + key},
			l.rate,
			l.burst,
			nowSec,
			1,
		).Result()

		if err == nil {
			if arr, ok := res.([]any); ok && len(arr) >= 2 {
				allowed := arr[0].(int64) == 1
				waitSec := arr[1].(int64)
				return allowed, time.Duration(waitSec) * time.Second
			}
		}
		// If Redis execution failed or timed out, gracefully fall through to in-memory
	}

	return l.allowInMemory(key)
}

func (l *Limiter) allowInMemory(key string) (bool, time.Duration) {
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

// Count returns the current number of tracked clients in-memory.
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

// UserOrIPKeyExtractor extracts userID if authenticated, otherwise client IP.
func UserOrIPKeyExtractor(c *gin.Context) string {
	if v, exists := c.Get("userID"); exists {
		if uid, ok := v.(string); ok && uid != "" {
			return "user:" + uid
		}
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
