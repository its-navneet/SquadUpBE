package api

import (
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (s *Server) GetUserRating(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	avg, n, attrs, e := s.ratingService.AverageWithAttributes(gid, mustUUID(c.Param("userId")))
	if e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"data": gin.H{
			"average":    avg,
			"count":      n,
			"attributes": attrs,
		},
	})
}

func (s *Server) UpsertUserRating(c *gin.Context) {
	var in struct {
		RatedUserID      string             `json:"rated_user_id"`
		RatedUserIDCamel string             `json:"ratedUserId"`
		Overall          float64            `json:"overall"`
		Attributes       map[string]float64 `json:"attributes"`
	}
	if c.BindJSON(&in) != nil {
		c.JSON(400, err("invalid request"))
		return
	}
	targetID := in.RatedUserID
	if targetID == "" {
		targetID = in.RatedUserIDCamel
	}
	ratedUID := mustUUID(targetID)
	if ratedUID == uuid.Nil {
		c.JSON(400, err("valid rated_user_id is required"))
		return
	}
	gid := mustUUID(c.Param("id"))
	if gid == uuid.Nil {
		c.JSON(400, err("invalid group id"))
		return
	}
	raterUID := mustUUID(auth.UserID(c))
	if raterUID == uuid.Nil {
		c.JSON(401, err("unauthorized"))
		return
	}
	p, e := s.ratingService.Upsert(gid, raterUID, ratedUID, in.Overall, in.Attributes)
	if e != nil {
		c.JSON(400, err(e.Error()))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:leaderboard:"+gid.String())
	c.JSON(201, gin.H{"success": true, "data": p})
}

func (s *Server) GetLeaderboard(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	cacheKey := "squadup:cache:leaderboard:" + gid.String()
	var st []models.PlayerStatistics
	if ok, _ := s.cache.Get(c.Request.Context(), cacheKey, &st); ok {
		c.JSON(200, gin.H{"success": true, "data": st})
		return
	}
	s.db.Where("group_id=?", gid).Order("goals DESC,assists DESC,matches DESC").Limit(50).Find(&st)
	_ = s.cache.Set(c.Request.Context(), cacheKey, st, 2*time.Minute)
	c.JSON(200, gin.H{"success": true, "data": st})
}
