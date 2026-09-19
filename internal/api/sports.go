package api

import (
	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
)

func (s *Server) ListSports(c *gin.Context) {
	var sp []models.Sport
	s.db.Where("active=true").Find(&sp)
	c.JSON(200, gin.H{"success": true, "data": sp})
}
