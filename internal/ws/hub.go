package ws

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"squadup/backend/internal/redisx"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Client struct {
	Conn   *websocket.Conn
	Key    string
	UserID string
	Mu     sync.Mutex
}

type Hub struct {
	mu          sync.RWMutex
	clients     map[string]map[*Client]struct{}
	redisClient *redisx.Client
	instanceID  string
	stopChan    chan struct{}
}

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type hubRedisPayload struct {
	InstanceID string `json:"instance_id"`
	Key        string `json:"key"`
	Event      Event  `json:"event"`
}

func New() *Hub {
	return NewWithRedis(nil)
}

func NewWithRedis(rc *redisx.Client) *Hub {
	h := &Hub{
		clients:     map[string]map[*Client]struct{}{},
		redisClient: rc,
		instanceID:  uuid.NewString(),
		stopChan:    make(chan struct{}),
	}

	if rc != nil && rc.IsAvailable() {
		go h.startRedisSubscriber()
	}

	return h
}

// AttachRedis binds a Redis client to an existing Hub and starts the subscriber.
func (h *Hub) AttachRedis(rc *redisx.Client) {
	h.mu.Lock()
	h.redisClient = rc
	h.mu.Unlock()

	if rc != nil && rc.IsAvailable() {
		go h.startRedisSubscriber()
	}
}

func (h *Hub) Add(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[c.Key] == nil {
		h.clients[c.Key] = map[*Client]struct{}{}
	}
	h.clients[c.Key][c] = struct{}{}
}

func (h *Hub) Remove(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.clients[c.Key]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(h.clients, c.Key)
		}
	}
}

// Broadcast sends the event to all local connected clients for the given key.
// If Redis is configured, it also publishes the event to Redis so other instances receive it.
func (h *Hub) Broadcast(key string, e Event) {
	// 1. Always deliver immediately to local clients
	h.broadcastLocal(key, e)

	// 2. If Redis is available, publish to remote instances
	h.mu.RLock()
	rc := h.redisClient
	instID := h.instanceID
	h.mu.RUnlock()

	if rc != nil && rc.IsAvailable() {
		payload := hubRedisPayload{
			InstanceID: instID,
			Key:        key,
			Event:      e,
		}
		data, err := json.Marshal(payload)
		if err == nil {
			_ = rc.Publish(context.Background(), "squadup:ws:"+key, data)
		}
	}
}

func (h *Hub) broadcastLocal(key string, e Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}

	h.mu.RLock()
	cs := make([]*Client, 0, len(h.clients[key]))
	for c := range h.clients[key] {
		cs = append(cs, c)
	}
	h.mu.RUnlock()

	for _, c := range cs {
		c.Mu.Lock()
		_ = c.Conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		_ = c.Conn.WriteMessage(websocket.TextMessage, b)
		c.Mu.Unlock()
	}
}

func (h *Hub) startRedisSubscriber() {
	h.mu.RLock()
	rc := h.redisClient
	h.mu.RUnlock()

	if rc == nil || !rc.IsAvailable() {
		return
	}

	pubsub := rc.PSubscribe(context.Background(), "squadup:ws:*")
	if pubsub == nil {
		return
	}
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-h.stopChan:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var p hubRedisPayload
			if err := json.Unmarshal([]byte(msg.Payload), &p); err != nil {
				continue
			}

			// Ignore events published by this instance since we already broadcasted locally
			if p.InstanceID == h.instanceID {
				continue
			}

			// Extract key from channel if not in payload
			key := p.Key
			if key == "" {
				key = strings.TrimPrefix(msg.Channel, "squadup:ws:")
			}

			h.broadcastLocal(key, p.Event)
		}
	}
}

// Stop terminates background subscribers.
func (h *Hub) Stop() {
	select {
	case <-h.stopChan:
	default:
		close(h.stopChan)
	}
}
