package redisx

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"squadup/backend/internal/config"

	"github.com/redis/go-redis/v9"
)

// Client wraps a redis.Client with availability checking and fallback support.
type Client struct {
	rdb       *redis.Client
	available bool
	mu        sync.RWMutex
}

// New creates a new Redis client based on config.
// If Redis is disabled, not configured, or unreachable, it gracefully marks the client
// as unavailable and logs a notice, allowing the application to use in-memory fallbacks.
func New(cfg *config.Config) *Client {
	if cfg == nil || !cfg.RedisEnabled || (cfg.RedisURL == "" && cfg.RedisAddr == "") {
		log.Println("[Redis] Redis is not configured or disabled; using in-memory fallbacks.")
		return &Client{available: false}
	}

	var opt *redis.Options
	var err error

	if cfg.RedisURL != "" {
		opt, err = redis.ParseURL(cfg.RedisURL)
		if err != nil {
			log.Printf("[Redis] Failed to parse REDIS_URL %q: %v. Falling back to in-memory mode.", cfg.RedisURL, err)
			return &Client{available: false}
		}
	} else {
		opt = &redis.Options{
			Addr:     cfg.RedisAddr,
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
		}
	}

	// Configure reasonable connection timeouts and pool sizes
	opt.DialTimeout = 3 * time.Second
	opt.ReadTimeout = 2 * time.Second
	opt.WriteTimeout = 2 * time.Second
	opt.PoolSize = 20
	opt.MinIdleConns = 5

	rdb := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("[Redis] Unable to connect to Redis at %s: %v. Operating with in-memory fallbacks.", opt.Addr, err)
		return &Client{
			rdb:       rdb,
			available: false,
		}
	}

	log.Printf("[Redis] Connected successfully to Redis (%s)", opt.Addr)
	return &Client{
		rdb:       rdb,
		available: true,
	}
}

// NewMock returns a Client explicitly flagged as available or unavailable (useful for testing).
func NewWithRDB(rdb *redis.Client, available bool) *Client {
	return &Client{
		rdb:       rdb,
		available: available,
	}
}

// IsAvailable returns true if Redis is configured and reachable.
func (c *Client) IsAvailable() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.available
}

// Raw returns the underlying *redis.Client (may be nil).
func (c *Client) Raw() *redis.Client {
	if c == nil {
		return nil
	}
	return c.rdb
}

// Ping checks if Redis is responsive.
func (c *Client) Ping(ctx context.Context) error {
	if !c.IsAvailable() {
		return fmt.Errorf("redis is not available")
	}
	return c.rdb.Ping(ctx).Err()
}

// Publish publishes a message to a Redis channel.
func (c *Client) Publish(ctx context.Context, channel string, message any) error {
	if !c.IsAvailable() {
		return fmt.Errorf("redis is not available")
	}
	return c.rdb.Publish(ctx, channel, message).Err()
}

// Subscribe creates a subscription to one or more channels.
func (c *Client) Subscribe(ctx context.Context, channels ...string) *redis.PubSub {
	if !c.IsAvailable() {
		return nil
	}
	return c.rdb.Subscribe(ctx, channels...)
}

// PSubscribe creates a pattern subscription.
func (c *Client) PSubscribe(ctx context.Context, patterns ...string) *redis.PubSub {
	if !c.IsAvailable() {
		return nil
	}
	return c.rdb.PSubscribe(ctx, patterns...)
}

// Get gets the string value of key.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	if !c.IsAvailable() {
		return "", fmt.Errorf("redis is not available")
	}
	return c.rdb.Get(ctx, key).Result()
}

// Set sets key to value with a given expiration TTL.
func (c *Client) Set(ctx context.Context, key string, val any, ttl time.Duration) error {
	if !c.IsAvailable() {
		return fmt.Errorf("redis is not available")
	}
	return c.rdb.Set(ctx, key, val, ttl).Err()
}

// SetNX sets key to value only if it does not already exist.
func (c *Client) SetNX(ctx context.Context, key string, val any, ttl time.Duration) (bool, error) {
	if !c.IsAvailable() {
		return false, fmt.Errorf("redis is not available")
	}
	return c.rdb.SetNX(ctx, key, val, ttl).Result()
}

// Del deletes one or more keys.
func (c *Client) Del(ctx context.Context, keys ...string) error {
	if !c.IsAvailable() || len(keys) == 0 {
		return nil
	}
	return c.rdb.Del(ctx, keys...).Err()
}

// Exists checks if a key exists.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	if !c.IsAvailable() {
		return false, fmt.Errorf("redis is not available")
	}
	n, err := c.rdb.Exists(ctx, key).Result()
	return n > 0, err
}

// Expire sets an expiration TTL on an existing key.
func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if !c.IsAvailable() {
		return fmt.Errorf("redis is not available")
	}
	return c.rdb.Expire(ctx, key, ttl).Err()
}

// ZAdd adds a member with a score to a sorted set.
func (c *Client) ZAdd(ctx context.Context, key string, score float64, member any) error {
	if !c.IsAvailable() {
		return fmt.Errorf("redis is not available")
	}
	return c.rdb.ZAdd(ctx, key, redis.Z{
		Score:  score,
		Member: member,
	}).Err()
}

// ZRevRangeWithScores returns elements in reverse order (highest score first) with scores.
func (c *Client) ZRevRangeWithScores(ctx context.Context, key string, start, stop int64) ([]redis.Z, error) {
	if !c.IsAvailable() {
		return nil, fmt.Errorf("redis is not available")
	}
	return c.rdb.ZRevRangeWithScores(ctx, key, start, stop).Result()
}

// ZRevRank returns the 0-based rank of member ordered highest to lowest.
func (c *Client) ZRevRank(ctx context.Context, key string, member string) (int64, error) {
	if !c.IsAvailable() {
		return -1, fmt.Errorf("redis is not available")
	}
	return c.rdb.ZRevRank(ctx, key, member).Result()
}

// ZScore returns the score of member in the sorted set.
func (c *Client) ZScore(ctx context.Context, key string, member string) (float64, error) {
	if !c.IsAvailable() {
		return 0, fmt.Errorf("redis is not available")
	}
	return c.rdb.ZScore(ctx, key, member).Result()
}

// ZIncrBy increments the score of member in the sorted set by increment.
func (c *Client) ZIncrBy(ctx context.Context, key string, increment float64, member string) (float64, error) {
	if !c.IsAvailable() {
		return 0, fmt.Errorf("redis is not available")
	}
	return c.rdb.ZIncrBy(ctx, key, increment, member).Result()
}

// SAdd adds members to a set.
func (c *Client) SAdd(ctx context.Context, key string, members ...any) error {
	if !c.IsAvailable() {
		return fmt.Errorf("redis is not available")
	}
	return c.rdb.SAdd(ctx, key, members...).Err()
}

// SRem removes members from a set.
func (c *Client) SRem(ctx context.Context, key string, members ...any) error {
	if !c.IsAvailable() {
		return fmt.Errorf("redis is not available")
	}
	return c.rdb.SRem(ctx, key, members...).Err()
}

// SMembers returns all members of a set.
func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	if !c.IsAvailable() {
		return nil, fmt.Errorf("redis is not available")
	}
	return c.rdb.SMembers(ctx, key).Result()
}

// SIsMember checks if a member exists in a set.
func (c *Client) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	if !c.IsAvailable() {
		return false, fmt.Errorf("redis is not available")
	}
	return c.rdb.SIsMember(ctx, key, member).Result()
}

// Close gracefully closes the Redis client connection pool.
func (c *Client) Close() error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.Close()
}
