package database

import (
	"log"
	"time"

	"squadup/backend/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func Open(dsn string) *gorm.DB {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		PrepareStmt:            true,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(50)
		sqlDB.SetMaxIdleConns(25)
		sqlDB.SetConnMaxLifetime(10 * time.Minute)
		sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	}
	return db
}
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&models.Sport{}, &models.SportPosition{}, &models.User{}, &models.SportProfile{},
		&models.Group{}, &models.GroupMember{}, &models.GroupJoinRequest{},
		&models.ChatMessage{}, &models.ChatMessageRead{}, &models.Venue{},
		&models.Match{}, &models.Attendance{}, &models.Team{}, &models.TeamMember{},
		&models.MatchEvent{}, &models.MatchResult{}, &models.PlayerRating{},
		&models.PlayerRatingAttribute{}, &models.PlayerStatistics{},
		&models.Notification{}, &models.DeviceToken{}, &models.AuditLog{},
		&models.Poll{}, &models.PollVote{}, &models.MiniMatch{},
	); err != nil {
		return err
	}

	// Performance optimization indexes for high-frequency queries
	indexes := []string{
		// Group members active filtering & user squads
		"CREATE INDEX IF NOT EXISTS idx_group_members_group_status ON group_members (group_id, status)",
		"CREATE INDEX IF NOT EXISTS idx_group_members_user_status ON group_members (user_id, status)",

		// Chat messages pagination and unread counts
		"CREATE INDEX IF NOT EXISTS idx_chat_messages_group_created ON chat_messages (group_id, created_at ASC)",
		"CREATE INDEX IF NOT EXISTS idx_chat_messages_group_sender ON chat_messages (group_id, sender_id)",
		"CREATE INDEX IF NOT EXISTS idx_chat_message_reads_user_group ON chat_message_reads (user_id, group_id)",

		// Matches scheduling, finalized filtering and venue foreign key
		"CREATE INDEX IF NOT EXISTS idx_matches_group_scheduled ON matches (group_id, scheduled_at DESC)",
		"CREATE INDEX IF NOT EXISTS idx_matches_group_finalized ON matches (group_id, finalized_at)",
		"CREATE INDEX IF NOT EXISTS idx_matches_venue_id ON matches (venue_id)",

		// Attendance filtering for squad generation
		"CREATE INDEX IF NOT EXISTS idx_attendances_match_status ON attendances (match_id, status)",

		// Match events timeline ordering
		"CREATE INDEX IF NOT EXISTS idx_match_events_match_time ON match_events (match_id, match_time_seconds ASC, created_at ASC)",

		// Player ratings squad & career lookups
		"CREATE INDEX IF NOT EXISTS idx_player_ratings_group_rated ON player_ratings (group_id, rated_user_id)",
		"CREATE INDEX IF NOT EXISTS idx_player_ratings_rated_user ON player_ratings (rated_user_id)",
		"CREATE INDEX IF NOT EXISTS idx_player_rating_attrs_rating_attr ON player_rating_attributes (player_rating_id, attribute)",

		// Player leaderboard ranking
		"CREATE INDEX IF NOT EXISTS idx_player_stats_leaderboard ON player_statistics (group_id, goals DESC, assists DESC, matches DESC)",

		// Notifications feed & unread count badge
		"CREATE INDEX IF NOT EXISTS idx_notifications_user_created ON notifications (user_id, created_at DESC)",
		"CREATE INDEX IF NOT EXISTS idx_notifications_user_read ON notifications (user_id, read_at)",

		// Polls & tournament fixtures
		"CREATE INDEX IF NOT EXISTS idx_polls_group_expires ON polls (group_id, expires_at, created_at DESC)",
		"CREATE INDEX IF NOT EXISTS idx_mini_matches_match_game ON mini_matches (match_id, game_number ASC)",

		// Group invite code case-insensitive lookup
		"CREATE INDEX IF NOT EXISTS idx_groups_lower_invite_code ON groups (LOWER(invite_code))",
	}

	for _, idx := range indexes {
		if err := db.Exec(idx).Error; err != nil {
			log.Printf("[Migrate] Note: index creation %q returned: %v", idx, err)
		}
	}

	return nil
}
