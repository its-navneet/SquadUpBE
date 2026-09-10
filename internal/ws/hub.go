package ws

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	Conn   *websocket.Conn
	Key    string
	UserID string
	Mu     sync.Mutex
}
type Hub struct {
	mu      sync.RWMutex
	clients map[string]map[*Client]struct{}
}
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

func New() *Hub { return &Hub{clients: map[string]map[*Client]struct{}{}} }
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
func (h *Hub) Broadcast(key string, e Event) {
	b, _ := json.Marshal(e)
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
