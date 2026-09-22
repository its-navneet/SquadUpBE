package api

import (
	"fmt"
	"log"
	"strings"
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"
	"squadup/backend/internal/ws"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm/clause"
)

func (s *Server) GetChatMessages(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	var msgs []models.ChatMessage
	if e := s.db.Where("group_id=?", gid).Order("created_at ASC").Limit(100).Find(&msgs).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	if len(msgs) == 0 {
		c.JSON(200, gin.H{"success": true, "data": []any{}})
		return
	}

	var msgIDs []uuid.UUID
	for _, m := range msgs {
		msgIDs = append(msgIDs, m.ID)
	}
	var reads []models.ChatMessageRead
	s.db.Where("message_id IN ?", msgIDs).Find(&reads)
	readsMap := make(map[uuid.UUID][]uuid.UUID)
	for _, r := range reads {
		readsMap[r.MessageID] = append(readsMap[r.MessageID], r.UserID)
	}

	type chatMsgResponse struct {
		models.ChatMessage
		SeenCount int         `json:"seen_count"`
		SeenByIDs []uuid.UUID `json:"seen_by_ids"`
	}
	out := make([]chatMsgResponse, len(msgs))
	for i, m := range msgs {
		userIDs := readsMap[m.ID]
		if userIDs == nil {
			userIDs = []uuid.UUID{}
		}
		out[i] = chatMsgResponse{
			ChatMessage: m,
			SeenCount:   len(userIDs),
			SeenByIDs:   userIDs,
		}
	}
	c.JSON(200, gin.H{"success": true, "data": out})
}

func (s *Server) MarkChatRead(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	var in struct {
		MessageIDs []string `json:"message_ids"`
	}
	_ = c.BindJSON(&in)

	var msgs []models.ChatMessage
	if len(in.MessageIDs) > 0 {
		var ids []uuid.UUID
		for _, sid := range in.MessageIDs {
			if id, errP := uuid.Parse(sid); errP == nil {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			s.db.Where("group_id=? AND id IN ? AND sender_id != ?", gid, ids, uid).Find(&msgs)
		}
	} else {
		s.db.Where("group_id=? AND sender_id != ?", gid, uid).Order("created_at DESC").Limit(100).Find(&msgs)
	}

	if len(msgs) == 0 {
		c.JSON(200, gin.H{"success": true, "data": gin.H{"marked_count": 0}})
		return
	}

	msgIDs := make([]uuid.UUID, len(msgs))
	for i, m := range msgs {
		msgIDs[i] = m.ID
	}

	var existingReadIDs []uuid.UUID
	s.db.Model(&models.ChatMessageRead{}).
		Where("user_id = ? AND message_id IN ?", uid, msgIDs).
		Pluck("message_id", &existingReadIDs)

	alreadyRead := make(map[uuid.UUID]bool, len(existingReadIDs))
	for _, id := range existingReadIDs {
		alreadyRead[id] = true
	}

	now := time.Now().UTC()
	var toInsert []models.ChatMessageRead
	var readIDs []uuid.UUID
	for _, m := range msgs {
		if !alreadyRead[m.ID] {
			toInsert = append(toInsert, models.ChatMessageRead{
				MessageID: m.ID,
				UserID:    uid,
				GroupID:   gid,
				ReadAt:    now,
			})
			readIDs = append(readIDs, m.ID)
		}
	}

	if len(toInsert) > 0 {
		_ = s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&toInsert)
	}

	if len(readIDs) > 0 {
		s.hub.Broadcast(gid.String(), ws.Event{
			Type: "MESSAGES_READ",
			Data: gin.H{
				"group_id":    gid,
				"user_id":     uid,
				"message_ids": readIDs,
				"read_at":     now,
			},
		})
	}

	c.JSON(200, gin.H{"success": true, "data": gin.H{"marked_count": len(readIDs)}})
}

func (s *Server) GetUnreadChatCount(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	var count int64
	s.db.Model(&models.ChatMessage{}).
		Where("group_id = ? AND sender_id != ?", gid, uid).
		Where("id NOT IN (SELECT message_id FROM chat_message_reads WHERE user_id = ? AND group_id = ?)", uid, gid).
		Count(&count)
	c.JSON(200, gin.H{"success": true, "data": gin.H{"unread_count": count}})
}

func (s *Server) SendTypingStatus(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	var in struct {
		IsTyping bool `json:"is_typing"`
	}
	if e := c.ShouldBindJSON(&in); e != nil {
		c.JSON(400, err("invalid input"))
		return
	}
	var user models.User
	s.db.Select("id, name").First(&user, uid)

	if in.IsTyping {
		_ = s.cache.Set(c.Request.Context(), "squadup:typing:"+gid.String()+":"+uid.String(), user.Name, 4*time.Second)
	} else {
		_ = s.cache.Delete(c.Request.Context(), "squadup:typing:"+gid.String()+":"+uid.String())
	}

	s.hub.Broadcast(gid.String(), ws.Event{
		Type: "USER_TYPING",
		Data: gin.H{
			"user_id":   uid.String(),
			"user_name": user.Name,
			"is_typing": in.IsTyping,
		},
	})
	c.JSON(200, gin.H{"success": true})
}

func (s *Server) GetSeenStatus(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	mid := mustUUID(c.Param("message_id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}

	var msg models.ChatMessage
	if s.db.Where("id=? AND group_id=?", mid, gid).First(&msg).Error != nil {
		c.JSON(404, err("message not found"))
		return
	}

	var reads []models.ChatMessageRead
	s.db.Where("message_id=?", mid).Order("read_at ASC").Find(&reads)

	type readReceiptItem struct {
		User   models.User `json:"user"`
		ReadAt time.Time   `json:"read_at"`
	}

	var members []models.GroupMember
	s.db.Where("group_id=? AND status='ACTIVE'", gid).Find(&members)

	// Batch lookup all referenced user profiles in a single query
	userIDs := make([]uuid.UUID, 0, len(reads)+len(members))
	for _, r := range reads {
		userIDs = append(userIDs, r.UserID)
	}
	for _, mb := range members {
		userIDs = append(userIDs, mb.UserID)
	}
	userMap := make(map[uuid.UUID]models.User)
	if len(userIDs) > 0 {
		var users []models.User
		s.db.Where("id IN ?", userIDs).Find(&users)
		for _, u := range users {
			userMap[u.ID] = u
		}
	}

	var seenBy []readReceiptItem
	readUserMap := make(map[uuid.UUID]bool)
	for _, r := range reads {
		if u, ok := userMap[r.UserID]; ok {
			seenBy = append(seenBy, readReceiptItem{
				User:   u,
				ReadAt: r.ReadAt,
			})
			readUserMap[r.UserID] = true
		}
	}

	var unseenBy []models.User
	for _, mb := range members {
		if mb.UserID == msg.SenderID {
			continue
		}
		if !readUserMap[mb.UserID] {
			if u, ok := userMap[mb.UserID]; ok {
				unseenBy = append(unseenBy, u)
			}
		}
	}

	if seenBy == nil {
		seenBy = []readReceiptItem{}
	}
	if unseenBy == nil {
		unseenBy = []models.User{}
	}

	c.JSON(200, gin.H{
		"success": true,
		"data": gin.H{
			"message_id": mid,
			"seen_by":    seenBy,
			"unseen_by":  unseenBy,
		},
	})
}

func (s *Server) SendChatMessage(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	var in struct {
		Content          string     `json:"content"`
		ReplyToMessageID *uuid.UUID `json:"reply_to_message_id"`
	}
	if c.BindJSON(&in) != nil || strings.TrimSpace(in.Content) == "" {
		c.JSON(400, err("message cannot be empty"))
		return
	}
	if len(in.Content) > 2000 {
		c.JSON(400, err("message exceeds maximum limit of 2000 characters"))
		return
	}
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	m := models.ChatMessage{
		GroupID:          gid,
		SenderID:         uid,
		MessageType:      "TEXT",
		Content:          in.Content,
		ReplyToMessageID: in.ReplyToMessageID,
	}
	if e := s.db.Create(&m).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	s.hub.Broadcast(gid.String(), ws.Event{Type: "CHAT_MESSAGE", Data: m})

	// Detect mentions of squad members and create in-app notifications
	go func(content string, senderUID uuid.UUID, groupID uuid.UUID) {
		var members []models.GroupMember
		s.db.Where("group_id=? AND status='ACTIVE'", groupID).Find(&members)
		var group models.Group
		if errVal := s.db.First(&group, groupID).Error; errVal != nil {
			log.Printf("[Chat Mention] Failed to find group %s: %v", groupID, errVal)
			return
		}
		var sender models.User
		if errVal := s.db.First(&sender, senderUID).Error; errVal != nil {
			log.Printf("[Chat Mention] Failed to find sender %s: %v", senderUID, errVal)
			return
		}

		var targetUserIDs []uuid.UUID
		for _, mb := range members {
			if mb.UserID != senderUID {
				targetUserIDs = append(targetUserIDs, mb.UserID)
			}
		}
		if len(targetUserIDs) == 0 {
			return
		}

		var targets []models.User
		s.db.Where("id IN ?", targetUserIDs).Find(&targets)

		contentLower := strings.ToLower(content)
		hasAllMention := strings.Contains(contentLower, "@all") || strings.Contains(contentLower, "@everyone") || strings.Contains(contentLower, "@squad")
		matchedCount := 0

		for _, target := range targets {
			targetName := strings.TrimSpace(target.Name)
			targetLower := strings.ToLower(targetName)
			nameParts := strings.Fields(targetLower)

			emailPrefix := ""
			if atIdx := strings.Index(target.Email, "@"); atIdx > 0 {
				emailPrefix = strings.ToLower(target.Email[:atIdx])
			}

			hasFullName := targetLower != "" && strings.Contains(contentLower, "@"+targetLower)
			hasFirstName := len(nameParts) > 0 && len(nameParts[0]) >= 2 && strings.Contains(contentLower, "@"+nameParts[0])
			hasEmailPrefix := emailPrefix != "" && len(emailPrefix) >= 2 && strings.Contains(contentLower, "@"+emailPrefix)

			if hasAllMention || hasFullName || hasFirstName || hasEmailPrefix {
				matchedCount++
				log.Printf("[Chat Mention] Matched user %s (%s) in group %s from sender %s", target.ID, target.Name, group.Name, sender.Name)
				snippet := content
				if len(snippet) > 80 {
					snippet = snippet[:77] + "..."
				}
				var title, msg string
				if hasAllMention {
					title = fmt.Sprintf("Tagged @everyone in %s", group.Name)
					msg = fmt.Sprintf("%s tagged @everyone in %s: \"%s\"", sender.Name, group.Name, snippet)
				} else {
					title = "Mentioned in squad chat"
					msg = fmt.Sprintf("%s tagged you in %s: \"%s\"", sender.Name, group.Name, snippet)
				}
				notif := models.Notification{
					UserID:     target.ID,
					GroupID:    &groupID,
					Type:       "CHAT_MENTION",
					Title:      title,
					Message:    msg,
					EntityType: "GROUP_CHAT",
					EntityID:   &groupID,
				}
				if errVal := s.db.Create(&notif).Error; errVal != nil {
					log.Printf("[Chat Mention] Failed to create notification for %s: %v", target.ID, errVal)
				}
			}
		}
		if matchedCount == 0 {
			log.Printf("[Chat Mention] No users matched mention query in message: %q", content)
		}
	}(in.Content, uid, gid)

	c.JSON(201, gin.H{"success": true, "data": m})
}

func (s *Server) DeleteChatMessage(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	mid := mustUUID(c.Param("messageId"))
	uid := mustUUID(auth.UserID(c))

	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}

	var m models.ChatMessage
	if e := s.db.Where("id = ? AND group_id = ?", mid, gid).First(&m).Error; e != nil {
		c.JSON(404, err("message not found"))
		return
	}

	isAdmin := isGroupAdmin(s.db, gid, uid)
	if m.SenderID != uid && !isAdmin {
		c.JSON(403, err("only message sender or squad admin can delete this message"))
		return
	}

	s.db.Where("message_id = ?", mid).Delete(&models.ChatMessageRead{})
	if e := s.db.Delete(&m).Error; e != nil {
		c.JSON(500, err("failed to delete message"))
		return
	}

	if s.hub != nil {
		s.hub.Broadcast(gid.String(), ws.Event{
			Type: "CHAT_MESSAGE_DELETED",
			Data: gin.H{
				"message_id": mid.String(),
				"group_id":   gid.String(),
			},
		})
	}

	c.JSON(200, gin.H{"success": true, "message": "message deleted"})
}
