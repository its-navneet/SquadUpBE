package database

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"log"
	"squadup/backend/internal/models"
)

func Open(dsn string) *gorm.DB {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	return db
}
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&models.Sport{}, &models.SportPosition{}, &models.User{}, &models.SportProfile{}, &models.Group{}, &models.GroupMember{}, &models.GroupJoinRequest{}, &models.ChatMessage{}, &models.Venue{}, &models.Match{}, &models.Attendance{}, &models.Team{}, &models.TeamMember{}, &models.MatchEvent{}, &models.MatchResult{}, &models.PlayerRating{}, &models.PlayerRatingAttribute{}, &models.PlayerStatistics{}, &models.Notification{}, &models.AuditLog{})
}
