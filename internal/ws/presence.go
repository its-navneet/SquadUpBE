package ws

import (
	"context"
	"sync"
	"time"

	"squadup/backend/internal/redisx"
)

// PresenceTracker tracks active online user sessions in a thread-safe manner,
// with optional Redis persistence for multi-instance deployments.
type PresenceTracker struct {
	mu          sync.RWMutex
	connections map[string]int // userID -> active connection count
	redisClient *redisx.Client
}

const (
	presenceTTL      = 2 * time.Minute
	redisOnlineSet   = "squadup:online_users"
	redisPresenceKey = "squadup:presence:"
)

func NewPresenceTracker() *PresenceTracker {
	return NewPresenceTrackerWithRedis(nil)
}

func NewPresenceTrackerWithRedis(rc *redisx.Client) *PresenceTracker {
	return &PresenceTracker{
		connections: make(map[string]int),
		redisClient: rc,
	}
}

// AttachRedis binds a Redis client to an existing PresenceTracker.
func (p *PresenceTracker) AttachRedis(rc *redisx.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.redisClient = rc
}

// Connect increments active connection count for userID.
// Returns true if the user just transitioned from offline to online.
func (p *PresenceTracker) Connect(userID string) bool {
	if userID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connections[userID]++
	isFirst := p.connections[userID] == 1

	if p.redisClient != nil && p.redisClient.IsAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = p.redisClient.Set(ctx, redisPresenceKey+userID, "1", presenceTTL)
		_ = p.redisClient.SAdd(ctx, redisOnlineSet, userID)
	}

	return isFirst
}

// Disconnect decrements active connection count for userID.
// Returns true if the user just transitioned from online to offline.
func (p *PresenceTracker) Disconnect(userID string) bool {
	if userID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	count, exists := p.connections[userID]
	if !exists {
		return false
	}

	if count <= 1 {
		delete(p.connections, userID)

		if p.redisClient != nil && p.redisClient.IsAvailable() {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_ = p.redisClient.Del(ctx, redisPresenceKey+userID)
			_ = p.redisClient.SRem(ctx, redisOnlineSet, userID)
		}

		return true
	}

	p.connections[userID] = count - 1

	if p.redisClient != nil && p.redisClient.IsAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = p.redisClient.Expire(ctx, redisPresenceKey+userID, presenceTTL)
	}

	return false
}

// IsOnline checks if a user has at least one active connection.
func (p *PresenceTracker) IsOnline(userID string) bool {
	p.mu.RLock()
	localOnline := p.connections[userID] > 0
	rc := p.redisClient
	p.mu.RUnlock()

	if localOnline {
		return true
	}

	if rc != nil && rc.IsAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		exists, err := rc.Exists(ctx, redisPresenceKey+userID)
		if err == nil && exists {
			return true
		}
	}

	return false
}

// OnlineUserIDs returns a list of all currently online user IDs.
func (p *PresenceTracker) OnlineUserIDs() []string {
	p.mu.RLock()
	rc := p.redisClient
	localMapLen := len(p.connections)
	p.mu.RUnlock()

	if rc != nil && rc.IsAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		members, err := rc.SMembers(ctx, redisOnlineSet)
		if err == nil && len(members) > 0 {
			return members
		}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	ids := make([]string, 0, localMapLen)
	for id := range p.connections {
		ids = append(ids, id)
	}
	return ids
}
