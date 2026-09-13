package api

import (
	"log"

	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
)

func (s *Server) ListSports(c *gin.Context) {
	var sp []models.Sport
	s.db.Where("active=true").Find(&sp)
	log.Println("/sports>>>>", sp)
	c.JSON(200, gin.H{"success": true, "data": sp})
}

