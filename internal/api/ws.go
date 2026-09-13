package api

import (
	"encoding/json"
	"net/http"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"
	"squadup/backend/internal/ws"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func (s *Server) GetOnlinePresence(c *gin.Context) {
	c.JSON(200, gin.H{
		"success": true,
		"data": gin.H{
			"online_user_ids": s.presence.OnlineUserIDs(),
		},
	})
}

func (s *Server) PresenceWS(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	if uid == uuid.Nil {
		c.AbortWithStatus(401)
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, e := up.Upgrade(c.Writer, c.Request, nil)
	if e != nil {
		return
	}
	cl := &ws.Client{Conn: conn, Key: "presence", UserID: uid.String()}
	s.hub.Add(cl)
	defer s.hub.Remove(cl)

	justCameOnline := s.presence.Connect(uid.String())
	if justCameOnline {
		s.hub.Broadcast("presence", ws.Event{
			Type: "USER_ONLINE",
			Data: gin.H{"user_id": uid.String()},
		})
	}

	// Initial sync of all currently active online users
	syncData, _ := json.Marshal(ws.Event{
		Type: "PRESENCE_SYNC",
		Data: gin.H{"online_user_ids": s.presence.OnlineUserIDs()},
	})
	cl.Mu.Lock()
	_ = conn.WriteMessage(websocket.TextMessage, syncData)
	cl.Mu.Unlock()

	defer func() {
		justWentOffline := s.presence.Disconnect(uid.String())
		if justWentOffline {
			s.hub.Broadcast("presence", ws.Event{
				Type: "USER_OFFLINE",
				Data: gin.H{"user_id": uid.String()},
			})
		}
	}()

	for {
		if _, _, e := conn.ReadMessage(); e != nil {
			return
		}
	}
}

func (s *Server) GroupWS(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.AbortWithStatus(403)
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, e := up.Upgrade(c.Writer, c.Request, nil)
	if e != nil {
		return
	}
	cl := &ws.Client{Conn: conn, Key: gid.String(), UserID: uid.String()}
	s.hub.Add(cl)
	defer s.hub.Remove(cl)

	justCameOnline := s.presence.Connect(uid.String())
	if justCameOnline {
		s.hub.Broadcast("presence", ws.Event{
			Type: "USER_ONLINE",
			Data: gin.H{"user_id": uid.String()},
		})
	}
	defer func() {
		justWentOffline := s.presence.Disconnect(uid.String())
		if justWentOffline {
			s.hub.Broadcast("presence", ws.Event{
				Type: "USER_OFFLINE",
				Data: gin.H{"user_id": uid.String()},
			})
		}
	}()

	for {
		_, raw, e := conn.ReadMessage()
		if e != nil {
			return
		}
		var in struct {
			Type     string `json:"type"`
			IsTyping bool   `json:"is_typing"`
		}
		if json.Unmarshal(raw, &in) == nil && (in.Type == "TYPING" || in.Type == "TYPING_START" || in.Type == "TYPING_STOP") {
			isTyping := in.IsTyping || in.Type == "TYPING_START"
			if in.Type == "TYPING_STOP" {
				isTyping = false
			}
			var user models.User
			s.db.Select("id, name").First(&user, uid)
			s.hub.Broadcast(gid.String(), ws.Event{
				Type: "USER_TYPING",
				Data: gin.H{
					"user_id":   uid.String(),
					"user_name": user.Name,
					"is_typing": isTyping,
				},
			})
		}
	}
}

func (s *Server) MatchWS(c *gin.Context) {
	var m models.Match
	if s.db.First(&m, mustUUID(c.Param("id"))).Error != nil {
		c.AbortWithStatus(404)
		return
	}
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, m.GroupID, uid) {
		c.AbortWithStatus(403)
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, e := up.Upgrade(c.Writer, c.Request, nil)
	if e != nil {
		return
	}
	cl := &ws.Client{Conn: conn, Key: m.ID.String(), UserID: uid.String()}
	s.hub.Add(cl)
	defer s.hub.Remove(cl)
	for {
		if _, _, e := conn.ReadMessage(); e != nil {
			return
		}
	}
}

