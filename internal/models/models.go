package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MediaSigner is an optional callback hook to generate presigned S3 URLs on JSON serialization.
var MediaSigner func(bucket, key, rawURL string) string

// ============================================================
// Base
// ============================================================

type Base struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BeforeCreate generates a UUID automatically if one is not set.
func (b *Base) BeforeCreate(tx *gorm.DB) error {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}

	return nil
}

// ============================================================
// Sport
// ============================================================

type Sport struct {
	Base

	Name   string `gorm:"uniqueIndex;not null" json:"name"`
	Icon   string `json:"icon"`
	Active bool   `gorm:"default:true" json:"active"`
}

// ============================================================
// Sport Position
// ============================================================

type SportPosition struct {
	Base

	SportID   uuid.UUID `gorm:"type:uuid;index;not null" json:"sport_id"`
	Name      string    `json:"name"`
	Code      string    `json:"code"`
	SortOrder int       `json:"sort_order"`
}

// ============================================================
// User
// ============================================================

type User struct {
	Base

	Name               string       `gorm:"not null" json:"name"`
	Email              string       `gorm:"uniqueIndex;not null" json:"email"`
	PasswordHash       string       `gorm:"not null" json:"-"`
	Age                int          `json:"age"`
	HeightCM           float64      `json:"height_cm"`
	WeightKG           float64      `json:"weight_kg"`
	ProfilePhotoURL    string       `json:"profile_photo_url"`
	ProfilePhotoBucket string       `json:"profile_photo_bucket,omitempty"`
	ProfilePhotoKey    string       `json:"profile_photo_key,omitempty"`
	PreferredFoot      string       `json:"preferred_foot"`
	Bio                string       `json:"bio"`
	Position           string       `json:"position"`
	KitNumber          int          `json:"kit_number"`
	FavouriteClub      string       `json:"favourite_club"`
	FavouritePlayer    string       `json:"favourite_player"`
	CareerMatches      int          `json:"career_matches"`
	CareerGoals        int          `json:"career_goals"`
	CareerAssists      int          `json:"career_assists"`
	CareerMVPs         int          `json:"career_mvps"`
	CareerStats        *CareerStats `gorm:"-" json:"career_stats,omitempty"`
}

func (u User) MarshalJSON() ([]byte, error) {
	type Alias User
	photo := u.ProfilePhotoURL
	if MediaSigner != nil && (u.ProfilePhotoKey != "" || photo != "") {
		photo = MediaSigner(u.ProfilePhotoBucket, u.ProfilePhotoKey, photo)
	}
	return json.Marshal(&struct {
		Alias
		ProfilePhotoURL string `json:"profile_photo_url"`
	}{
		Alias:           Alias(u),
		ProfilePhotoURL: photo,
	})
}

type CareerStats struct {
	Matches     int     `json:"matches"`
	Wins        int     `json:"wins"`
	Draws       int     `json:"draws"`
	Losses      int     `json:"losses"`
	Goals       int     `json:"goals"`
	Assists     int     `json:"assists"`
	MVP         int     `json:"mvp"`
	CleanSheets int     `json:"clean_sheets"`
	WinRate     float64 `json:"win_rate"`
}

// ============================================================
// Sport Profile
// ============================================================

type SportProfile struct {
	Base

	UserID              uuid.UUID  `gorm:"type:uuid;index;not null" json:"user_id"`
	SportID             uuid.UUID  `gorm:"type:uuid;index;not null" json:"sport_id"`
	PrimaryPositionID   *uuid.UUID `gorm:"type:uuid" json:"primary_position_id,omitempty"`
	SecondaryPositionID *uuid.UUID `gorm:"type:uuid" json:"secondary_position_id,omitempty"`
	Metadata            string     `gorm:"type:jsonb" json:"metadata,omitempty"`
}

// ============================================================
// Group
// ============================================================

type Group struct {
	Base

	Name        string    `gorm:"not null" json:"name"`
	Description string    `json:"description"`
	LogoURL     string    `json:"logo_url"`
	LogoBucket  string    `json:"logo_bucket,omitempty"`
	LogoKey     string    `json:"logo_key,omitempty"`
	City        string    `json:"city"`
	SportID     uuid.UUID `gorm:"type:uuid;index;not null" json:"sport_id"`
	Privacy     string    `gorm:"default:'PRIVATE'" json:"privacy"`
	OwnerID     uuid.UUID `gorm:"type:uuid;index;not null" json:"owner_id"`
	InviteCode  string    `gorm:"uniqueIndex;not null" json:"invite_code"`
}

func (g Group) MarshalJSON() ([]byte, error) {
	type Alias Group
	logo := g.LogoURL
	if MediaSigner != nil && (g.LogoKey != "" || logo != "") {
		logo = MediaSigner(g.LogoBucket, g.LogoKey, logo)
	}
	return json.Marshal(&struct {
		Alias
		LogoURL string `json:"logo_url"`
	}{
		Alias:   Alias(g),
		LogoURL: logo,
	})
}

// ============================================================
// Group Member
// ============================================================

type GroupMember struct {
	Base

	GroupID  uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_group_member" json:"group_id"`
	UserID   uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_group_member" json:"user_id"`
	Role     string    `gorm:"default:'MEMBER'" json:"role"`
	Status   string    `gorm:"default:'ACTIVE'" json:"status"`
	JoinedAt time.Time `json:"joined_at"`
}

// ============================================================
// Group Join Request
// ============================================================

type GroupJoinRequest struct {
	Base

	GroupID      uuid.UUID  `gorm:"type:uuid;index;not null" json:"group_id"`
	UserID       uuid.UUID  `gorm:"type:uuid;index;not null" json:"user_id"`
	Status       string     `gorm:"default:'PENDING'" json:"status"`
	ReviewedByID *uuid.UUID `gorm:"type:uuid" json:"reviewed_by_id,omitempty"`
}

// ============================================================
// Chat Message
// ============================================================

type ChatMessage struct {
	Base

	GroupID          uuid.UUID      `gorm:"type:uuid;index;not null" json:"group_id"`
	SenderID         uuid.UUID      `gorm:"type:uuid;index;not null" json:"sender_id"`
	MessageType      string         `gorm:"default:'TEXT'" json:"message_type"`
	Content          string         `json:"content"`
	MediaURL         string         `json:"media_url,omitempty"`
	ReplyToMessageID *uuid.UUID     `gorm:"type:uuid" json:"reply_to_message_id,omitempty"`
	DeletedAt        gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// ============================================================
// Chat Message Read Receipt
// ============================================================

type ChatMessageRead struct {
	Base

	MessageID uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_message_user_read" json:"message_id"`
	UserID    uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_message_user_read" json:"user_id"`
	GroupID   uuid.UUID `gorm:"type:uuid;index;not null" json:"group_id"`
	ReadAt    time.Time `json:"read_at"`
}

// ============================================================
// Venue
// ============================================================

type Venue struct {
	Base

	GroupID       uuid.UUID `gorm:"type:uuid;index;not null" json:"group_id"`
	Name          string    `gorm:"not null" json:"name"`
	Address       string    `json:"address"`
	GoogleMapsURL string    `json:"google_maps_url"`
	Latitude      *float64  `json:"latitude,omitempty"`
	Longitude     *float64  `json:"longitude,omitempty"`
	Notes         string    `json:"notes,omitempty"`
	IsDefault     bool      `gorm:"default:false" json:"is_default"`
}

// ============================================================
// Match
// ============================================================

type Match struct {
	Base

	GroupID         uuid.UUID  `gorm:"type:uuid;index;not null" json:"group_id"`
	SportID         uuid.UUID  `gorm:"type:uuid;index;not null" json:"sport_id"`
	VenueID         *uuid.UUID `gorm:"type:uuid" json:"venue_id,omitempty"`
	Venue           string     `json:"venue,omitempty"`
	VenueMapURL     string     `json:"venue_map_url,omitempty"`
	Name            string     `gorm:"not null" json:"name"`
	ScheduledAt     time.Time  `gorm:"not null" json:"scheduled_at"`
	DurationMinutes int        `json:"duration_minutes"`
	Format          string     `json:"format"`
	TeamCount       int        `json:"team_count"`
	PlayersPerTeam  int        `json:"players_per_team"`
	MaxPlayers      int        `json:"max_players"`
	Notes           string     `json:"notes,omitempty"`
	Status          string     `gorm:"default:'UPCOMING'" json:"status"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	FinalizedAt     *time.Time `json:"finalized_at,omitempty"`
	PosterURL       string     `json:"poster_url,omitempty"`
	PosterBucket    string     `json:"poster_bucket,omitempty"`
	PosterKey       string     `json:"poster_key,omitempty"`
}

func (m Match) MarshalJSON() ([]byte, error) {
	type Alias Match
	poster := m.PosterURL
	if MediaSigner != nil && (m.PosterKey != "" || poster != "") {
		poster = MediaSigner(m.PosterBucket, m.PosterKey, poster)
	}
	return json.Marshal(&struct {
		Alias
		PosterURL string `json:"poster_url,omitempty"`
	}{
		Alias:     Alias(m),
		PosterURL: poster,
	})
}

// ============================================================
// Attendance
// ============================================================

type Attendance struct {
	Base

	MatchID     uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_attendance_match_user" json:"match_id"`
	UserID      uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_attendance_match_user" json:"user_id"`
	Status      string    `gorm:"not null" json:"status"`
	RespondedAt time.Time `json:"responded_at"`
}

// ============================================================
// Team
// ============================================================

type Team struct {
	Base

	MatchID  uuid.UUID `gorm:"type:uuid;index;not null" json:"match_id"`
	Name     string    `json:"name"`
	Code     string    `json:"code"`
	Strength float64   `json:"strength"`
}

// ============================================================
// Team Member
// ============================================================

type TeamMember struct {
	Base

	TeamID       uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_team_member" json:"team_id"`
	UserID       uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_team_member" json:"user_id"`
	PositionName string    `json:"position_name"`
}

// ============================================================
// Match Event
// ============================================================

type MatchEvent struct {
	Base

	MatchID          uuid.UUID  `gorm:"type:uuid;index;not null" json:"match_id"`
	TeamID           *uuid.UUID `gorm:"type:uuid;index" json:"team_id,omitempty"`
	PlayerID         *uuid.UUID `gorm:"type:uuid;index" json:"player_id,omitempty"`
	AssistPlayerID   *uuid.UUID `gorm:"type:uuid;index" json:"assist_player_id,omitempty"`
	EventType        string     `gorm:"not null" json:"event_type"`
	MatchTimeSeconds int        `json:"match_time_seconds"`
	Metadata         string     `gorm:"type:jsonb" json:"metadata,omitempty"`
}

// ============================================================
// Match Result
// ============================================================

type MatchResult struct {
	Base

	MatchID   uuid.UUID  `gorm:"type:uuid;uniqueIndex;not null" json:"match_id"`
	HomeScore int        `gorm:"default:0" json:"home_score"`
	AwayScore int        `gorm:"default:0" json:"away_score"`
	MVPUserID *uuid.UUID `gorm:"type:uuid" json:"mvp_user_id,omitempty"`
	Notes     string     `json:"notes,omitempty"`
}

// ============================================================
// Player Rating
// ============================================================

type PlayerRating struct {
	Base

	GroupID     uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_player_rating" json:"group_id"`
	RaterUserID uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_player_rating" json:"rater_user_id"`
	RatedUserID uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_player_rating" json:"rated_user_id"`
	Overall     float64   `gorm:"not null" json:"overall"`
}

// ============================================================
// Player Rating Attribute
// ============================================================

type PlayerRatingAttribute struct {
	Base

	PlayerRatingID uuid.UUID `gorm:"type:uuid;index;not null" json:"player_rating_id"`
	Attribute      string    `json:"attribute"`
	Value          float64   `json:"value"`
}

// ============================================================
// Player Statistics
// ============================================================

type PlayerStatistics struct {
	Base

	GroupID         uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_group_user_stats" json:"group_id"`
	UserID          uuid.UUID `gorm:"type:uuid;index;not null;uniqueIndex:idx_group_user_stats" json:"user_id"`
	Matches         int       `json:"matches"`
	Wins            int       `json:"wins"`
	Losses          int       `json:"losses"`
	Draws           int       `json:"draws"`
	Goals           int       `json:"goals"`
	Assists         int       `json:"assists"`
	MVP             int       `json:"mvp"`
	CleanSheets     int       `json:"clean_sheets"`
	YellowCards     int       `json:"yellow_cards"`
	RedCards        int       `json:"red_cards"`
	AttendanceCount int       `json:"attendance_count"`
	AttendanceTotal int       `json:"attendance_total"`
}

// ============================================================
// Notification
// ============================================================

type Notification struct {
	Base

	UserID     uuid.UUID  `gorm:"type:uuid;index;not null" json:"user_id"`
	GroupID    *uuid.UUID `gorm:"type:uuid" json:"group_id,omitempty"`
	Type       string     `json:"type"`
	Title      string     `json:"title"`
	Message    string     `json:"message"`
	EntityType string     `json:"entity_type"`
	EntityID   *uuid.UUID `gorm:"type:uuid" json:"entity_id,omitempty"`
	ReadAt     *time.Time `json:"read_at,omitempty"`
}

// ============================================================
// Audit Log
// ============================================================

type AuditLog struct {
	Base

	ActorUserID uuid.UUID  `gorm:"type:uuid;index;not null" json:"actor_user_id"`
	GroupID     *uuid.UUID `gorm:"type:uuid" json:"group_id,omitempty"`
	Action      string     `json:"action"`
	EntityType  string     `json:"entity_type"`
	EntityID    *uuid.UUID `gorm:"type:uuid" json:"entity_id,omitempty"`
	Metadata    string     `gorm:"type:jsonb" json:"metadata,omitempty"`
}
