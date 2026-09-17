package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"
)

type CreatePollRequest struct {
	Title           string `json:"title"`
	MatchDate       string `json:"match_date"`
	DurationMinutes int    `json:"duration_minutes"`
	Venue           string `json:"venue"`
	ExpiresAt       string `json:"expires_at"`
}

type VotePollRequest struct {
	Option string `json:"option"` // 'IN', 'OUT', 'MAYBE'
}

type PollVoterDTO struct {
	UserID          string `json:"user_id"`
	Name            string `json:"name"`
	ProfilePhotoURL string `json:"profile_photo_url"`
	Option          string `json:"option"`
}

type PollResponseDTO struct {
	ID              string         `json:"id"`
	GroupID         string         `json:"group_id"`
	CreatorID       string         `json:"creator_id"`
	Title           string         `json:"title"`
	MatchDate       string         `json:"match_date"`
	DurationMinutes int            `json:"duration_minutes"`
	Venue           string         `json:"venue"`
	ExpiresAt       string         `json:"expires_at"`
	Status          string         `json:"status"`
	InCount         int            `json:"in_count"`
	OutCount        int            `json:"out_count"`
	MaybeCount      int            `json:"maybe_count"`
	TotalVotes      int            `json:"total_votes"`
	MyVote          *string        `json:"my_vote"`
	Voters          []PollVoterDTO `json:"voters"`
}

func (s *Server) CreatePoll(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if gid == uuid.Nil || uid == uuid.Nil {
		c.JSON(400, err("invalid group or user id"))
		return
	}

	if !mustAdmin(s.db, gid, uid) {
		c.JSON(403, err("only group owner or admins can create polls"))
		return
	}

	var in CreatePollRequest
	if errBind := c.BindJSON(&in); errBind != nil {
		c.JSON(400, err("invalid poll request body"))
		return
	}

	matchTime, e := time.Parse(time.RFC3339, in.MatchDate)
	if e != nil {
		c.JSON(400, err("invalid match_date, expected RFC3339 format"))
		return
	}

	expiresAt, e := time.Parse(time.RFC3339, in.ExpiresAt)
	if e != nil {
		c.JSON(400, err("invalid expires_at, expected RFC3339 format"))
		return
	}

	if !expiresAt.After(time.Now()) {
		c.JSON(400, err("expires_at must be in the future"))
		return
	}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "Match Availability"
	}
	duration := in.DurationMinutes
	if duration <= 0 {
		duration = 60
	}

	var g models.Group
	if e := s.db.First(&g, gid).Error; e != nil {
		c.JSON(404, err("group not found"))
		return
	}

	// Clean up any existing polls for this group to keep strictly 1 active poll
	var oldPolls []models.Poll
	if s.db.Where("group_id = ?", gid).Find(&oldPolls).Error == nil && len(oldPolls) > 0 {
		var oldIDs []uuid.UUID
		for _, p := range oldPolls {
			oldIDs = append(oldIDs, p.ID)
		}
		s.db.Where("poll_id IN (?)", oldIDs).Delete(&models.PollVote{})
		s.db.Where("id IN (?)", oldIDs).Delete(&models.Poll{})
	}

	poll := models.Poll{
		GroupID:         gid,
		CreatorID:       uid,
		Title:           title,
		MatchDate:       matchTime,
		DurationMinutes: duration,
		Venue:           strings.TrimSpace(in.Venue),
		ExpiresAt:       expiresAt,
		Status:          "ACTIVE",
	}

	if errCreate := s.db.Create(&poll).Error; errCreate != nil {
		c.JSON(500, err("failed to create poll: "+errCreate.Error()))
		return
	}

	// Dispatch push notifications to all group members (excluding creator)
	var creator models.User
	creatorName := "Squad Admin"
	if s.db.Select("name").First(&creator, uid).Error == nil && creator.Name != "" {
		creatorName = creator.Name
	}

	formattedMatchTime := matchTime.Format("Mon, Jan 02 • 15:04")
	notifTitle := fmt.Sprintf("Match Poll: %s", g.Name)
	notifMsg := fmt.Sprintf("%s asked: Are you in for match on %s? Vote now!", creatorName, formattedMatchTime)

	notifyGroupMembers(s.db, gid, &uid, "MATCH_POLL_CREATED", notifTitle, notifMsg, "POLL", &poll.ID)

	res := s.buildPollDTO(poll, uid)
	c.JSON(201, gin.H{"success": true, "data": res})
}

func (s *Server) GetActivePoll(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if gid == uuid.Nil || uid == uuid.Nil {
		c.JSON(400, err("invalid group or user id"))
		return
	}

	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}

	// Purge and delete any expired polls for this group
	s.cleanupExpiredPolls(gid)

	var poll models.Poll
	if e := s.db.Where("group_id = ? AND expires_at > ?", gid, time.Now()).Order("created_at DESC").First(&poll).Error; e != nil {
		c.JSON(200, gin.H{"success": true, "data": nil})
		return
	}

	res := s.buildPollDTO(poll, uid)
	c.JSON(200, gin.H{"success": true, "data": res})
}

func (s *Server) VotePoll(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	pid := mustUUID(c.Param("pollId"))
	uid := mustUUID(auth.UserID(c))
	if gid == uuid.Nil || pid == uuid.Nil || uid == uuid.Nil {
		c.JSON(400, err("invalid parameters"))
		return
	}

	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}

	var poll models.Poll
	if e := s.db.Where("id = ? AND group_id = ?", pid, gid).First(&poll).Error; e != nil {
		c.JSON(404, err("poll not found"))
		return
	}

	if time.Now().After(poll.ExpiresAt) {
		// Poll is expired -> delete it
		s.cleanupExpiredPolls(gid)
		c.JSON(410, err("poll has expired"))
		return
	}

	var in VotePollRequest
	if e := c.BindJSON(&in); e != nil {
		c.JSON(400, err("invalid vote payload"))
		return
	}

	opt := strings.ToUpper(strings.TrimSpace(in.Option))
	if opt != "IN" && opt != "OUT" && opt != "MAYBE" {
		c.JSON(400, err("invalid option, must be IN, OUT, or MAYBE"))
		return
	}

	var existingVote models.PollVote
	if s.db.Where("poll_id = ? AND user_id = ?", poll.ID, uid).First(&existingVote).Error == nil {
		existingVote.Option = opt
		if e := s.db.Save(&existingVote).Error; e != nil {
			c.JSON(500, err("failed to update vote"))
			return
		}
	} else {
		newVote := models.PollVote{
			PollID: poll.ID,
			UserID: uid,
			Option: opt,
		}
		if e := s.db.Create(&newVote).Error; e != nil {
			c.JSON(500, err("failed to record vote"))
			return
		}
	}

	res := s.buildPollDTO(poll, uid)
	c.JSON(200, gin.H{"success": true, "data": res})
}

func (s *Server) DeletePoll(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	pid := mustUUID(c.Param("pollId"))
	uid := mustUUID(auth.UserID(c))
	if gid == uuid.Nil || pid == uuid.Nil || uid == uuid.Nil {
		c.JSON(400, err("invalid parameters"))
		return
	}

	if !mustAdmin(s.db, gid, uid) {
		c.JSON(403, err("only group owner or admins can delete polls"))
		return
	}

	s.db.Where("poll_id = ?", pid).Delete(&models.PollVote{})
	s.db.Where("id = ? AND group_id = ?", pid, gid).Delete(&models.Poll{})

	c.JSON(200, gin.H{"success": true})
}

func (s *Server) cleanupExpiredPolls(gid uuid.UUID) {
	now := time.Now()
	var expired []models.Poll
	if s.db.Where("group_id = ? AND expires_at <= ?", gid, now).Find(&expired).Error == nil && len(expired) > 0 {
		var expiredIDs []uuid.UUID
		for _, p := range expired {
			expiredIDs = append(expiredIDs, p.ID)
		}
		s.db.Where("poll_id IN (?)", expiredIDs).Delete(&models.PollVote{})
		s.db.Where("id IN (?)", expiredIDs).Delete(&models.Poll{})
	}
}

func (s *Server) buildPollDTO(poll models.Poll, currentUserID uuid.UUID) PollResponseDTO {
	var votes []models.PollVote
	s.db.Where("poll_id = ?", poll.ID).Find(&votes)

	var inCount, outCount, maybeCount int
	var myVote *string
	userIDs := make([]uuid.UUID, 0, len(votes))
	voteByUser := make(map[uuid.UUID]string)

	for _, v := range votes {
		userIDs = append(userIDs, v.UserID)
		voteByUser[v.UserID] = v.Option
		switch v.Option {
		case "IN":
			inCount++
		case "OUT":
			outCount++
		case "MAYBE":
			maybeCount++
		}
		if v.UserID == currentUserID {
			optCopy := v.Option
			myVote = &optCopy
		}
	}

	votersDTO := make([]PollVoterDTO, 0, len(userIDs))
	if len(userIDs) > 0 {
		var users []models.User
		s.db.Where("id IN (?)", userIDs).Find(&users)
		for _, u := range users {
			opt := voteByUser[u.ID]
			photo := u.ProfilePhotoURL
			if models.MediaSigner != nil && (u.ProfilePhotoKey != "" || photo != "") {
				photo = models.MediaSigner(u.ProfilePhotoBucket, u.ProfilePhotoKey, photo)
			}
			votersDTO = append(votersDTO, PollVoterDTO{
				UserID:          u.ID.String(),
				Name:            u.Name,
				ProfilePhotoURL: photo,
				Option:          opt,
			})
		}
	}

	return PollResponseDTO{
		ID:              poll.ID.String(),
		GroupID:         poll.GroupID.String(),
		CreatorID:       poll.CreatorID.String(),
		Title:           poll.Title,
		MatchDate:       poll.MatchDate.Format(time.RFC3339),
		DurationMinutes: poll.DurationMinutes,
		Venue:           poll.Venue,
		ExpiresAt:       poll.ExpiresAt.Format(time.RFC3339),
		Status:          poll.Status,
		InCount:         inCount,
		OutCount:        outCount,
		MaybeCount:      maybeCount,
		TotalVotes:      len(votes),
		MyVote:          myVote,
		Voters:          votersDTO,
	}
}
