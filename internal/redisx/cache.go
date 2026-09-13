package redisx

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type inMemCacheEntry struct {
	data      []byte
	expiresAt time.Time
}

// Cache provides convenient JSON caching backed by Redis with in-memory fallback.
type Cache struct {
	client   *Client
	inMemMu  sync.RWMutex
	inMemMap map[string]inMemCacheEntry
}

// NewCache creates a new Cache.
func NewCache(client *Client) *Cache {
	c := &Cache{
		client:   client,
		inMemMap: make(map[string]inMemCacheEntry),
	}
	return c
}

// Get retrieves and deserializes cached JSON into dest.
// Returns (true, nil) if found and deserialized, (false, nil) on cache miss.
func (c *Cache) Get(ctx context.Context, key string, dest any) (bool, error) {
	if c.client != nil && c.client.IsAvailable() {
		val, err := c.client.Get(ctx, key)
		if err != nil {
			return false, nil // cache miss
		}
		if err := json.Unmarshal([]byte(val), dest); err != nil {
			return false, fmt.Errorf("failed to unmarshal cached JSON: %w", err)
		}
		return true, nil
	}

	// In-memory fallback
	c.inMemMu.RLock()
	entry, exists := c.inMemMap[key]
	c.inMemMu.RUnlock()

	if !exists {
		return false, nil
	}
	if time.Now().After(entry.expiresAt) {
		c.inMemMu.Lock()
		delete(c.inMemMap, key)
		c.inMemMu.Unlock()
		return false, nil
	}

	if err := json.Unmarshal(entry.data, dest); err != nil {
		return false, fmt.Errorf("failed to unmarshal in-memory cached JSON: %w", err)
	}
	return true, nil
}

// Set serializes val to JSON and caches it with TTL.
func (c *Cache) Set(ctx context.Context, key string, val any, ttl time.Duration) error {
	data, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("failed to marshal value for cache: %w", err)
	}

	if c.client != nil && c.client.IsAvailable() {
		return c.client.Set(ctx, key, string(data), ttl)
	}

	// In-memory fallback
	c.inMemMu.Lock()
	defer c.inMemMu.Unlock()
	c.inMemMap[key] = inMemCacheEntry{
		data:      data,
		expiresAt: time.Now().Add(ttl),
	}
	return nil
}

// Delete removes one or more keys from the cache.
func (c *Cache) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}

	if c.client != nil && c.client.IsAvailable() {
		_ = c.client.Del(ctx, keys...)
	}

	c.inMemMu.Lock()
	defer c.inMemMu.Unlock()
	for _, k := range keys {
		delete(c.inMemMap, k)
	}
	return nil
}
