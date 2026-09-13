package api

import (
	"net/url"
	"strings"

	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func err(msg string) gin.H {
	return gin.H{"success": false, "error": gin.H{"message": msg}}
}

func mustUUID(s string) uuid.UUID {
	x, e := uuid.Parse(s)
	if e != nil {
		// return Nil instead of panicking; callers should validate where appropriate
		return uuid.Nil
	}
	return x
}

func defaultStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func defaultInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func mustMember(db *gorm.DB, gid, uid uuid.UUID) bool {
	var c int64
	db.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND status='ACTIVE'", gid, uid).Count(&c)
	return c > 0
}

func isGroupAdmin(db *gorm.DB, gid, uid uuid.UUID) bool {
	var g models.Group
	if db.Select("owner_id").First(&g, gid).Error == nil && g.OwnerID == uid {
		return true
	}
	var m models.GroupMember
	return db.Where("group_id=? AND user_id=? AND status='ACTIVE' AND role IN ?", gid, uid, []string{"OWNER", "ADMIN"}).First(&m).Error == nil
}

func mustAdmin(db *gorm.DB, gid, uid uuid.UUID) bool {
	var g models.Group
	if db.Select("owner_id").First(&g, gid).Error == nil && g.OwnerID == uid {
		return true
	}
	var m models.GroupMember
	if db.Where("group_id=? AND user_id=? AND status='ACTIVE'", gid, uid).First(&m).Error != nil {
		return false
	}
	return m.Role == "OWNER" || m.Role == "ADMIN"
}

func mustMatchMember(db *gorm.DB, mid, uid uuid.UUID) bool {
	var m models.Match
	if db.First(&m, mid).Error != nil {
		return false
	}
	return mustMember(db, m.GroupID, uid)
}

func notifyGroupMembers(db *gorm.DB, groupID uuid.UUID, excludeUserID *uuid.UUID, notifType, title, message, entityType string, entityID *uuid.UUID) {
	var members []models.GroupMember
	q := db.Where("group_id = ? AND status = 'ACTIVE'", groupID)
	if excludeUserID != nil && *excludeUserID != uuid.Nil {
		q = q.Where("user_id != ?", *excludeUserID)
	}
	if err := q.Find(&members).Error; err != nil {
		return
	}
	for _, mem := range members {
		n := models.Notification{
			UserID:     mem.UserID,
			GroupID:    &groupID,
			Type:       notifType,
			Title:      title,
			Message:    message,
			EntityType: entityType,
			EntityID:   entityID,
		}
		db.Create(&n)
	}
}

func cleanStoredURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Scheme != "" {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	return raw
}

