package ws

import (
	"sync"
)

// PresenceTracker tracks active online user sessions in a thread-safe manner.
type PresenceTracker struct {
	mu          sync.RWMutex
	connections map[string]int // userID -> active connection count
}

func NewPresenceTracker() *PresenceTracker {
	return &PresenceTracker{
		connections: make(map[string]int),
	}
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
	return p.connections[userID] == 1
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
		return true
	}
	p.connections[userID] = count - 1
	return false
}

// IsOnline checks if a user has at least one active connection.
func (p *PresenceTracker) IsOnline(userID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connections[userID] > 0
}

// OnlineUserIDs returns a list of all currently online user IDs.
func (p *PresenceTracker) OnlineUserIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ids := make([]string, 0, len(p.connections))
	for id := range p.connections {
		ids = append(ids, id)
	}
	return ids
}
