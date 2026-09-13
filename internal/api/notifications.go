package api

import (
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (s *Server) ListNotifications(c *gin.Context) {
	notifications := []models.Notification{}
	s.db.Where("user_id=?", mustUUID(auth.UserID(c))).Order("created_at DESC").Limit(50).Find(&notifications)
	c.JSON(200, gin.H{"success": true, "data": notifications})
}

func (s *Server) GetUnreadNotificationCount(c *gin.Context) {
	var count int64
	s.db.Model(&models.Notification{}).Where("user_id=? AND read_at IS NULL", mustUUID(auth.UserID(c))).Count(&count)
	c.JSON(200, gin.H{"success": true, "data": gin.H{"unread_count": count}})
}

func (s *Server) MarkNotificationRead(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	nid := mustUUID(c.Param("id"))
	if nid == uuid.Nil {
		c.JSON(400, err("invalid notification id"))
		return
	}
	now := time.Now()
	s.db.Model(&models.Notification{}).Where("id=? AND user_id=?", nid, uid).Update("read_at", now)
	c.JSON(200, gin.H{"success": true})
}

func (s *Server) MarkAllNotificationsRead(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	now := time.Now()
	s.db.Model(&models.Notification{}).Where("user_id=? AND read_at IS NULL", uid).Update("read_at", now)
	c.JSON(200, gin.H{"success": true})
}

func (s *Server) DeleteNotification(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	nid := mustUUID(c.Param("id"))
	if nid == uuid.Nil {
		c.JSON(400, err("invalid notification id"))
		return
	}
	s.db.Where("id=? AND user_id=?", nid, uid).Delete(&models.Notification{})
	c.JSON(200, gin.H{"success": true})
}

func (s *Server) ClearAllNotifications(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	s.db.Where("user_id=?", uid).Delete(&models.Notification{})
	c.JSON(200, gin.H{"success": true})
}

