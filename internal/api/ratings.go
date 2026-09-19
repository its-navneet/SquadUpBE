package api

import (
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
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
		RatedUserID string
		Overall     float64
		Attributes  map[string]float64
	}
	if c.BindJSON(&in) != nil {
		c.JSON(400, err("invalid request"))
		return
	}
	gid := mustUUID(c.Param("id"))
	p, e := s.ratingService.Upsert(gid, mustUUID(auth.UserID(c)), mustUUID(in.RatedUserID), in.Overall, in.Attributes)
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
