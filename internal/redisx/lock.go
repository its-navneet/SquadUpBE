package redisx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// luaReleaseLock atomically compares the lock token with the key's value and deletes the key if matched.
var luaReleaseLock = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
else
    return 0
end
`)

// Locker provides distributed locking backed by Redis, with in-memory mutex fallback.
type Locker struct {
	client   *Client
	inMemMu  sync.Mutex
	inMemMap map[string]string // key -> token
}

// NewLocker creates a Locker.
func NewLocker(client *Client) *Locker {
	return &Locker{
		client:   client,
		inMemMap: make(map[string]string),
	}
}

func generateToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Acquire attempts to acquire a lock for the given key with a specified TTL.
// Returns a token (to release the lock) and true if acquired, or false if already held.
func (l *Locker) Acquire(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	token := generateToken()

	if l.client != nil && l.client.IsAvailable() {
		ok, err := l.client.SetNX(ctx, "lock:"+key, token, ttl)
		if err != nil {
			return "", false, err
		}
		return token, ok, nil
	}

	// In-memory fallback
	l.inMemMu.Lock()
	defer l.inMemMu.Unlock()

	if _, held := l.inMemMap[key]; held {
		return "", false, nil
	}

	l.inMemMap[key] = token

	// Auto-expire in memory after TTL
	go func(k, tok string, d time.Duration) {
		time.Sleep(d)
		l.inMemMu.Lock()
		defer l.inMemMu.Unlock()
		if cur, exists := l.inMemMap[k]; exists && cur == tok {
			delete(l.inMemMap, k)
		}
	}(key, token, ttl)

	return token, true, nil
}

// Release releases a previously acquired lock if the token matches.
func (l *Locker) Release(ctx context.Context, key string, token string) (bool, error) {
	if token == "" {
		return false, nil
	}

	if l.client != nil && l.client.IsAvailable() {
		res, err := luaReleaseLock.Run(ctx, l.client.Raw(), []string{"lock:" + key}, token).Result()
		if err != nil {
			return false, err
		}
		if count, ok := res.(int64); ok && count > 0 {
			return true, nil
		}
		return false, nil
	}

	// In-memory fallback
	l.inMemMu.Lock()
	defer l.inMemMu.Unlock()

	if cur, exists := l.inMemMap[key]; exists && cur == token {
		delete(l.inMemMap, key)
		return true, nil
	}
	return false, nil
}
