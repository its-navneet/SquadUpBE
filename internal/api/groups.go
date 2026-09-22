package api

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"
	"squadup/backend/internal/storage"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (s *Server) CreateGroup(c *gin.Context) {
	var in struct {
		Name, Description, City, Privacy string
		SportID                          string `json:"sport_id"`
	}
	if c.BindJSON(&in) != nil || in.Name == "" {
		c.JSON(400, err("group name is required"))
		return
	}
	uid := mustUUID(auth.UserID(c))
	if strings.TrimSpace(in.SportID) == "" {
		c.JSON(400, err("sport_id is required"))
		return
	}
	sidParsed, sidErr := uuid.Parse(in.SportID)
	if sidErr != nil {
		c.JSON(400, err("invalid sport_id"))
		return
	}
	sid := sidParsed
	g, e := s.groupService.Create(in.Name, in.Description, in.City, strings.ToUpper(defaultStr(in.Privacy, "PRIVATE")), sid, uid)
	if e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	c.JSON(201, gin.H{"success": true, "data": g})
}

func (s *Server) ListGroups(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	var gsx []models.Group
	s.db.Joins("JOIN group_members gm ON gm.group_id=groups.id").Where("gm.user_id=? AND gm.status='ACTIVE'", uid).Order("groups.created_at DESC").Find(&gsx)
	c.JSON(200, gin.H{"success": true, "data": gsx})
}

func (s *Server) GetGroup(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	var g models.Group
	if e := s.db.First(&g, gid).Error; e != nil {
		c.JSON(404, err("group not found"))
		return
	}
	var unreadCount int64
	s.db.Model(&models.ChatMessage{}).
		Where("group_id = ? AND sender_id != ?", gid, uid).
		Where("id NOT IN (SELECT message_id FROM chat_message_reads WHERE user_id = ? AND group_id = ?)", uid, gid).
		Count(&unreadCount)
	c.JSON(200, gin.H{"success": true, "data": g, "unread_count": unreadCount})
}

func (s *Server) UpdateGroup(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !isGroupAdmin(s.db, gid, uid) {
		c.JSON(403, err("admin access required"))
		return
	}
	var g models.Group
	if e := s.db.First(&g, gid).Error; e != nil {
		c.JSON(404, err("group not found"))
		return
	}
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		City        *string `json:"city"`
		LogoURL     *string `json:"logo_url"`
		LogoBucket  *string `json:"logo_bucket"`
		LogoKey     *string `json:"logo_key"`
	}
	if c.ShouldBindJSON(&in) != nil {
		c.JSON(400, err("invalid request"))
		return
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		g.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		g.Description = strings.TrimSpace(*in.Description)
	}
	if in.City != nil {
		g.City = strings.TrimSpace(*in.City)
	}
	var oldLogoKey, oldLogoBucket string
	if in.LogoURL != nil || in.LogoKey != nil {
		var newBucket, newKey, newCleanURL string
		if in.LogoKey != nil && *in.LogoKey != "" {
			newKey = strings.TrimSpace(*in.LogoKey)
			newBucket = s.storage.BucketName()
			if in.LogoBucket != nil && *in.LogoBucket != "" {
				newBucket = strings.TrimSpace(*in.LogoBucket)
			}
			newCleanURL = newKey
		} else if in.LogoURL != nil {
			raw := strings.TrimSpace(*in.LogoURL)
			if raw != "" {
				newBucket, newKey = storage.ExtractBucketAndKey(raw, s.storage.BucketName())
				newCleanURL = cleanStoredURL(raw)
			}
		}

		oldKey := g.LogoKey
		oldBucket := g.LogoBucket
		if oldKey == "" && g.LogoURL != "" {
			oldBucket, oldKey = storage.ExtractBucketAndKey(g.LogoURL, s.storage.BucketName())
		}
		if oldBucket == "" {
			oldBucket = s.storage.BucketName()
		}

		if oldKey != "" && oldKey != newKey {
			oldLogoKey = oldKey
			oldLogoBucket = oldBucket
		}

		g.LogoBucket = newBucket
		g.LogoKey = newKey
		g.LogoURL = newCleanURL
	}
	if e := s.db.Save(&g).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	if oldLogoKey != "" {
		go func(b, k string) {
			delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if delErr := s.storage.DeleteObject(delCtx, b, k); delErr != nil {
				log.Printf("[Storage] Failed to delete previous squad logo %s/%s: %v", b, k, delErr)
			} else {
				log.Printf("[Storage] Deleted previous squad logo %s/%s", b, k)
			}
		}(oldLogoBucket, oldLogoKey)
	}
	c.JSON(200, gin.H{"success": true, "data": g})
}

func (s *Server) ListMembers(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	var msx []models.GroupMember
	s.db.Where("group_id=? AND status='ACTIVE'", gid).Find(&msx)
	var ids []uuid.UUID
	for _, m := range msx {
		ids = append(ids, m.UserID)
	}
	var us []models.User
	if len(ids) > 0 {
		s.db.Where("id IN ?", ids).Find(&us)
		type UserCareerAgg struct {
			UserID      uuid.UUID `gorm:"column:user_id"`
			Matches     int       `gorm:"column:matches"`
			Wins        int       `gorm:"column:wins"`
			Draws       int       `gorm:"column:draws"`
			Losses      int       `gorm:"column:losses"`
			Goals       int       `gorm:"column:goals"`
			Assists     int       `gorm:"column:assists"`
			MVP         int       `gorm:"column:mvp"`
			CleanSheets int       `gorm:"column:clean_sheets"`
			Saves       int       `gorm:"column:saves"`
			Fouls       int       `gorm:"column:fouls"`
		}
		var aggs []UserCareerAgg
		s.db.Table("player_statistics").
			Select("user_id, SUM(matches) as matches, SUM(wins) as wins, SUM(draws) as draws, SUM(losses) as losses, SUM(goals) as goals, SUM(assists) as assists, SUM(mvp) as mvp, SUM(clean_sheets) as clean_sheets, SUM(saves) as saves, SUM(fouls) as fouls").
			Where("user_id IN ?", ids).
			Group("user_id").
			Scan(&aggs)
		aggMap := make(map[uuid.UUID]UserCareerAgg)
		for _, a := range aggs {
			aggMap[a.UserID] = a
		}
		for i := range us {
			if a, ok := aggMap[us[i].ID]; ok {
				winRate := 0.0
				if a.Matches > 0 {
					winRate = math.Round((float64(a.Wins)/float64(a.Matches))*1000) / 10
				}
				us[i].CareerStats = &models.CareerStats{
					Matches:     a.Matches,
					Wins:        a.Wins,
					Draws:       a.Draws,
					Losses:      a.Losses,
					Goals:       a.Goals,
					Assists:     a.Assists,
					MVP:         a.MVP,
					CleanSheets: a.CleanSheets,
					Saves:       a.Saves,
					Fouls:       a.Fouls,
					WinRate:     winRate,
				}
			} else {
				us[i].CareerStats = &models.CareerStats{}
			}
		}
	}
	c.JSON(200, gin.H{"success": true, "data": gin.H{"members": msx, "users": us}})
}

func (s *Server) UpdateMemberRole(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	targetUID := mustUUID(c.Param("userId"))
	actorUID := mustUUID(auth.UserID(c))

	if !isGroupAdmin(s.db, gid, actorUID) {
		c.JSON(403, err("admin access required"))
		return
	}

	var in struct {
		Role string `json:"role"`
	}
	if e := c.ShouldBindJSON(&in); e != nil {
		c.JSON(400, err("invalid input"))
		return
	}
	newRole := strings.ToUpper(strings.TrimSpace(in.Role))
	if newRole != "ADMIN" && newRole != "MEMBER" {
		c.JSON(400, err("role must be ADMIN or MEMBER"))
		return
	}

	var targetMember models.GroupMember
	if e := s.db.Where("group_id=? AND user_id=? AND status='ACTIVE'", gid, targetUID).First(&targetMember).Error; e != nil {
		c.JSON(404, err("member not found in group"))
		return
	}

	if targetMember.Role == "OWNER" {
		c.JSON(400, err("cannot change owner role"))
		return
	}

	var g models.Group
	if s.db.First(&g, gid).Error != nil {
		c.JSON(404, err("group not found"))
		return
	}

	// Only the group OWNER can promote to ADMIN or demote an existing ADMIN
	if targetMember.Role == "ADMIN" || newRole == "ADMIN" {
		if g.OwnerID != actorUID {
			c.JSON(403, err("only the group owner can promote or demote admins"))
			return
		}
	}

	targetMember.Role = newRole
	if e := s.db.Save(&targetMember).Error; e != nil {
		c.JSON(500, err("failed to update member role"))
		return
	}

	groupName := g.Name
	if groupName == "" {
		groupName = "the squad"
	}

	var notifTitle, notifMsg string
	if newRole == "ADMIN" {
		notifTitle = "Promoted to Admin"
		notifMsg = "You are now an admin of " + groupName + "."
	} else {
		notifTitle = "Role Updated"
		notifMsg = "Your role in " + groupName + " is now Member."
	}

	n := models.Notification{
		UserID:     targetUID,
		GroupID:    &gid,
		Type:       "ROLE_UPDATED",
		Title:      notifTitle,
		Message:    notifMsg,
		EntityType: "GROUP",
		EntityID:   &gid,
	}
	_ = s.db.Create(&n).Error

	c.JSON(200, gin.H{"success": true, "data": targetMember})
}

func (s *Server) RemoveMember(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	currentUID := mustUUID(auth.UserID(c))
	targetUID := mustUUID(c.Param("userId"))

	var targetMember models.GroupMember
	if e := s.db.Where("group_id = ? AND user_id = ? AND status = 'ACTIVE'", gid, targetUID).First(&targetMember).Error; e != nil {
		c.JSON(404, err("member not found in group"))
		return
	}

	var g models.Group
	if e := s.db.First(&g, gid).Error; e != nil {
		c.JSON(404, err("group not found"))
		return
	}

	isSelf := currentUID == targetUID
	if isSelf {
		if g.OwnerID == currentUID {
			c.JSON(400, err("squad owner cannot leave the squad. Transfer ownership or delete the squad."))
			return
		}
		targetMember.Status = "LEFT"
		if e := s.db.Save(&targetMember).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		notifyGroupMembers(s.db, gid, &currentUID, "MEMBER_LEFT", "Member Left", "A member has left the squad.", "GROUP", &gid)
		c.JSON(200, gin.H{"success": true, "message": "left squad successfully"})
		return
	}

	if !isGroupAdmin(s.db, gid, currentUID) {
		c.JSON(403, err("admin access required to remove members"))
		return
	}
	if targetUID == g.OwnerID {
		c.JSON(400, err("cannot remove the squad owner"))
		return
	}
	if targetMember.Role == "ADMIN" && currentUID != g.OwnerID {
		c.JSON(403, err("only the squad owner can remove an admin"))
		return
	}

	targetMember.Status = "REMOVED"
	if e := s.db.Save(&targetMember).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}

	n := models.Notification{
		UserID:     targetUID,
		GroupID:    &gid,
		Type:       "MEMBER_REMOVED",
		Title:      "Removed from Squad",
		Message:    "You have been removed from " + g.Name + ".",
		EntityType: "GROUP",
		EntityID:   &gid,
	}
	_ = s.db.Create(&n).Error

	c.JSON(200, gin.H{"success": true, "message": "member removed successfully"})
}

func (s *Server) ListJoinRequests(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !isGroupAdmin(s.db, gid, uid) {
		c.JSON(403, err("admin access required"))
		return
	}
	var requests []models.GroupJoinRequest
	s.db.Where("group_id=? AND status='PENDING'", gid).Order("created_at ASC").Find(&requests)
	// Older app versions could create duplicates; show only the oldest one.
	uniqueRequests := make([]models.GroupJoinRequest, 0, len(requests))
	seenUsers := make(map[uuid.UUID]bool)
	for _, request := range requests {
		if !seenUsers[request.UserID] {
			seenUsers[request.UserID] = true
			uniqueRequests = append(uniqueRequests, request)
		}
	}
	requests = uniqueRequests
	var userIDs []uuid.UUID
	for _, request := range requests {
		userIDs = append(userIDs, request.UserID)
	}
	var users []models.User
	if len(userIDs) > 0 {
		s.db.Where("id IN ?", userIDs).Find(&users)
	}
	c.JSON(200, gin.H{"success": true, "data": gin.H{"requests": requests, "users": users}})
}

func (s *Server) JoinGroup(c *gin.Context) {
	var in struct {
		InviteCode string `json:"invite_code"`
	}

	if errVal := c.ShouldBindJSON(&in); errVal != nil {
		c.JSON(400, gin.H{
			"success": false,
			"error":   "invalid request body",
			"details": errVal.Error(),
		})
		return
	}

	if strings.TrimSpace(in.InviteCode) == "" {
		c.JSON(400, gin.H{
			"success": false,
			"error":   "invite code required",
		})
		return
	}

	r, e := s.groupService.JoinByCode(
		strings.TrimSpace(in.InviteCode),
		mustUUID(auth.UserID(c)),
	)

	if e != nil {
		c.JSON(400, err(e.Error()))
		return
	}

	var g models.Group
	s.db.First(&g, r.GroupID)

	c.JSON(201, gin.H{
		"success": true,
		"data": gin.H{
			"id":             r.ID,
			"group_id":       r.GroupID,
			"user_id":        r.UserID,
			"status":         r.Status,
			"created_at":     r.CreatedAt,
			"group_name":     g.Name,
			"group_logo_url": g.LogoURL,
			"group_city":     g.City,
		},
	})
}

func (s *Server) GetMyJoinRequests(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	var requests []models.GroupJoinRequest
	s.db.Where("user_id=? AND status='PENDING'", uid).Order("created_at DESC").Find(&requests)

	type JoinRequestWithGroup struct {
		ID           uuid.UUID `json:"id"`
		GroupID      uuid.UUID `json:"group_id"`
		UserID       uuid.UUID `json:"user_id"`
		Status       string    `json:"status"`
		CreatedAt    time.Time `json:"created_at"`
		GroupName    string    `json:"group_name"`
		GroupLogoURL string    `json:"group_logo_url"`
		GroupCity    string    `json:"group_city"`
	}

	groupIDs := make([]uuid.UUID, 0, len(requests))
	for _, req := range requests {
		groupIDs = append(groupIDs, req.GroupID)
	}

	activeMemberships := make(map[uuid.UUID]bool)
	groupsMap := make(map[uuid.UUID]models.Group)
	if len(groupIDs) > 0 {
		var activeGroupIDs []uuid.UUID
		s.db.Model(&models.GroupMember{}).
			Where("user_id = ? AND group_id IN ? AND status = 'ACTIVE'", uid, groupIDs).
			Pluck("group_id", &activeGroupIDs)
		for _, gid := range activeGroupIDs {
			activeMemberships[gid] = true
		}

		var groups []models.Group
		s.db.Where("id IN ?", groupIDs).Find(&groups)
		for _, g := range groups {
			groupsMap[g.ID] = g
		}
	}

	result := make([]JoinRequestWithGroup, 0, len(requests))
	for _, req := range requests {
		status := req.Status
		if activeMemberships[req.GroupID] {
			status = "APPROVED"
		}

		g := groupsMap[req.GroupID]
		result = append(result, JoinRequestWithGroup{
			ID:           req.ID,
			GroupID:      req.GroupID,
			UserID:       req.UserID,
			Status:       status,
			CreatedAt:    req.CreatedAt,
			GroupName:    g.Name,
			GroupLogoURL: g.LogoURL,
			GroupCity:    g.City,
		})
	}

	c.JSON(200, gin.H{"success": true, "data": result})
}

func (s *Server) GetJoinRequestStatus(c *gin.Context) {
	rid := mustUUID(c.Param("requestId"))
	uid := mustUUID(auth.UserID(c))

	var req models.GroupJoinRequest
	if e := s.db.First(&req, rid).Error; e != nil {
		c.JSON(404, err("join request not found"))
		return
	}
	if req.UserID != uid {
		c.JSON(403, err("forbidden"))
		return
	}

	status := req.Status
	var count int64
	s.db.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND status='ACTIVE'", req.GroupID, uid).Count(&count)
	if count > 0 {
		status = "APPROVED"
	}

	var g models.Group
	s.db.First(&g, req.GroupID)

	c.JSON(200, gin.H{
		"success": true,
		"data": gin.H{
			"id":             req.ID,
			"group_id":       req.GroupID,
			"user_id":        req.UserID,
			"status":         status,
			"created_at":     req.CreatedAt,
			"group_name":     g.Name,
			"group_logo_url": g.LogoURL,
			"group_city":     g.City,
		},
	})
}

func (s *Server) CancelJoinRequest(c *gin.Context) {
	rid := mustUUID(c.Param("requestId"))
	uid := mustUUID(auth.UserID(c))

	var req models.GroupJoinRequest
	if e := s.db.First(&req, rid).Error; e != nil {
		c.JSON(404, err("join request not found"))
		return
	}
	if req.UserID != uid {
		c.JSON(403, err("forbidden"))
		return
	}
	if req.Status != "PENDING" {
		c.JSON(400, err("only pending requests can be cancelled"))
		return
	}

	if e := s.db.Delete(&req).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	c.JSON(200, gin.H{"success": true})
}

func (s *Server) ApproveJoinRequest(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	var request models.GroupJoinRequest
	if e := s.db.First(&request, mustUUID(c.Param("requestId"))).Error; e != nil || request.GroupID != gid {
		c.JSON(404, err("join request not found"))
		return
	}
	r, e := s.groupService.Approve(mustUUID(c.Param("requestId")), mustUUID(auth.UserID(c)))
	if e != nil {
		c.JSON(403, err(e.Error()))
		return
	}
	var g models.Group
	s.db.First(&g, gid)
	var newUser models.User
	s.db.First(&newUser, request.UserID)
	userName := newUser.Name
	if userName == "" {
		userName = "A new player"
	}
	notifyGroupMembers(s.db, gid, &request.UserID, "MEMBER_JOINED", "New Squad Member", fmt.Sprintf("%s has joined %s! Welcome them to the squad.", userName, g.Name), "GROUP", &gid)
	c.JSON(200, gin.H{"success": true, "data": r})
}

func (s *Server) RejectJoinRequest(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	var request models.GroupJoinRequest
	if e := s.db.First(&request, mustUUID(c.Param("requestId"))).Error; e != nil || request.GroupID != gid {
		c.JSON(404, err("join request not found"))
		return
	}
	r, e := s.groupService.Reject(mustUUID(c.Param("requestId")), mustUUID(auth.UserID(c)))
	if e != nil {
		c.JSON(403, err(e.Error()))
		return
	}
	c.JSON(200, gin.H{"success": true, "data": r})
}

func (s *Server) DeleteGroup(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))

	var g models.Group
	if e := s.db.First(&g, gid).Error; e != nil {
		c.JSON(404, err("group not found"))
		return
	}

	if g.OwnerID != uid {
		c.JSON(403, err("only the squad owner can delete this squad"))
		return
	}

	errTx := s.db.Transaction(func(tx *gorm.DB) error {
		// Find all match IDs
		var matchIDs []uuid.UUID
		tx.Model(&models.Match{}).Where("group_id = ?", gid).Pluck("id", &matchIDs)

		if len(matchIDs) > 0 {
			// Find team IDs
			var teamIDs []uuid.UUID
			tx.Model(&models.Team{}).Where("match_id IN ?", matchIDs).Pluck("id", &teamIDs)
			if len(teamIDs) > 0 {
				tx.Where("team_id IN ?", teamIDs).Delete(&models.TeamMember{})
				tx.Where("id IN ?", teamIDs).Delete(&models.Team{})
			}
			tx.Where("match_id IN ?", matchIDs).Delete(&models.MatchEvent{})
			tx.Where("match_id IN ?", matchIDs).Delete(&models.MatchResult{})
			tx.Where("match_id IN ?", matchIDs).Delete(&models.Attendance{})
			tx.Where("match_id IN ?", matchIDs).Delete(&models.MiniMatch{})
			tx.Where("id IN ?", matchIDs).Delete(&models.Match{})
		}

		// Delete polls & votes
		var pollIDs []uuid.UUID
		tx.Model(&models.Poll{}).Where("group_id = ?", gid).Pluck("id", &pollIDs)
		if len(pollIDs) > 0 {
			tx.Where("poll_id IN ?", pollIDs).Delete(&models.PollVote{})
			tx.Where("id IN ?", pollIDs).Delete(&models.Poll{})
		}

		// Delete ratings & attributes
		var ratingIDs []uuid.UUID
		tx.Model(&models.PlayerRating{}).Where("group_id = ?", gid).Pluck("id", &ratingIDs)
		if len(ratingIDs) > 0 {
			tx.Where("player_rating_id IN ?", ratingIDs).Delete(&models.PlayerRatingAttribute{})
			tx.Where("id IN ?", ratingIDs).Delete(&models.PlayerRating{})
		}

		// Delete chat
		tx.Where("group_id = ?", gid).Delete(&models.ChatMessageRead{})
		tx.Where("group_id = ?", gid).Delete(&models.ChatMessage{})

		// Delete venues, statistics, join requests, members, group
		tx.Where("group_id = ?", gid).Delete(&models.Venue{})
		tx.Where("group_id = ?", gid).Delete(&models.PlayerStatistics{})
		tx.Where("group_id = ?", gid).Delete(&models.GroupJoinRequest{})
		tx.Where("group_id = ?", gid).Delete(&models.GroupMember{})
		return tx.Delete(&g).Error
	})

	if errTx != nil {
		c.JSON(500, err("failed to delete squad: "+errTx.Error()))
		return
	}

	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:leaderboard:"+gid.String())
	c.JSON(200, gin.H{"success": true, "message": "squad deleted successfully"})
}
