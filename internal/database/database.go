package database

import (
	"log"
	"time"

	"squadup/backend/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func Open(dsn string) *gorm.DB {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(50)
		sqlDB.SetMaxIdleConns(25)
		sqlDB.SetConnMaxLifetime(5 * time.Minute)
	}
	return db
}
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&models.Sport{}, &models.SportPosition{}, &models.User{}, &models.SportProfile{}, &models.Group{}, &models.GroupMember{}, &models.GroupJoinRequest{}, &models.ChatMessage{}, &models.ChatMessageRead{}, &models.Venue{}, &models.Match{}, &models.Attendance{}, &models.Team{}, &models.TeamMember{}, &models.MatchEvent{}, &models.MatchResult{}, &models.PlayerRating{}, &models.PlayerRatingAttribute{}, &models.PlayerStatistics{}, &models.Notification{}, &models.DeviceToken{}, &models.AuditLog{}, &models.Poll{}, &models.PollVote{}, &models.MiniMatch{})
}
