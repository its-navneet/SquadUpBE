package api

import (
	"context"
	"strings"
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (s *Server) MarkAttendance(c *gin.Context) {
	m := c.MustGet("match").(models.Match)
	if m.Status != "UPCOMING" {
		c.JSON(400, err("attendance marking is closed for this match"))
		return
	}
	mid := m.ID
	token, acquired, _ := s.locker.Acquire(c.Request.Context(), "attendance:"+mid.String(), 5*time.Second)
	if acquired {
		defer s.locker.Release(context.Background(), "attendance:"+mid.String(), token)
	}
	uid := mustUUID(auth.UserID(c))
	var in struct {
		Status string  `json:"status"`
		UserID *string `json:"user_id"`
	}
	if c.ShouldBindJSON(&in) != nil {
		c.JSON(400, err("invalid request"))
		return
	}
	st := strings.ToUpper(strings.TrimSpace(in.Status))
	if st != "GOING" && st != "MAYBE" && st != "NOT_GOING" {
		c.JSON(400, err("invalid attendance status"))
		return
	}

	targetUID := uid
	if in.UserID != nil && strings.TrimSpace(*in.UserID) != "" {
		if parsedUID, errP := uuid.Parse(strings.TrimSpace(*in.UserID)); errP == nil && parsedUID != uuid.Nil && parsedUID != uid {
			if !isGroupAdmin(s.db, m.GroupID, uid) {
				c.JSON(403, err("only squad admins can record attendance for other players"))
				return
			}
			targetUID = parsedUID
		}
	}

	if !mustMatchMember(s.db, mid, targetUID) {
		c.JSON(403, err("target player is not a squad member"))
		return
	}
	// Cleanly remove any existing attendance for this match & user to prevent duplicates
	s.db.Where("match_id = ? AND user_id = ?", mid, targetUID).Delete(&models.Attendance{})
	a := models.Attendance{
		MatchID:     mid,
		UserID:      targetUID,
		Status:      st,
		RespondedAt: time.Now(),
	}
	if e := s.db.Create(&a).Error; e != nil {
		c.JSON(500, err("could not record attendance"))
		return
	}
	if st != "GOING" {
		// If member is no longer GOING, remove them from any generated squads for this upcoming match
		var match models.Match
		if errVal := s.db.First(&match, mid).Error; errVal == nil && match.Status == "UPCOMING" {
			s.db.Exec(`
				DELETE FROM team_members 
				WHERE user_id = ? AND team_id IN (
					SELECT id FROM teams WHERE match_id = ?
				)
			`, targetUID, mid)
		}
	}
	var u models.User
	s.db.First(&u, targetUID)
	c.JSON(200, gin.H{"success": true, "data": a, "user": u})
}

func (s *Server) GetAttendance(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	var ax []models.Attendance
	if e := s.db.Where("match_id=?", mid).Order("responded_at DESC, created_at DESC").Find(&ax).Error; e != nil {
		c.JSON(500, err("could not load attendance"))
		return
	}
	// Deduplicate by user_id keeping the newest response
	seenUsers := make(map[uuid.UUID]bool)
	deduped := make([]models.Attendance, 0, len(ax))
	for _, a := range ax {
		if !seenUsers[a.UserID] {
			seenUsers[a.UserID] = true
			deduped = append(deduped, a)
		}
	}
	ax = deduped
	var uids []uuid.UUID
	for _, a := range ax {
		uids = append(uids, a.UserID)
	}
	var users []models.User
	if len(uids) > 0 {
		s.db.Where("id IN ?", uids).Find(&users)
	}
	userMap := make(map[uuid.UUID]models.User)
	for _, u := range users {
		userMap[u.ID] = u
	}
	type itemOut struct {
		models.Attendance
		User *models.User `json:"user,omitempty"`
	}
	out := make([]itemOut, 0, len(ax))
	for _, a := range ax {
		var uPtr *models.User
		if u, ok := userMap[a.UserID]; ok {
			uCopy := u
			uPtr = &uCopy
		}
		out = append(out, itemOut{Attendance: a, User: uPtr})
	}
	c.JSON(200, gin.H{"success": true, "data": out, "users": users})
}
