package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Server) ServePosterImage(c *gin.Context) {
	mid := c.Param("id")
	if _, parseErr := uuid.Parse(mid); parseErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid match id"})
		return
	}
	posterPath := filepath.Join("uploads", "match-posters", fmt.Sprintf("%s.png", mid))
	if _, err := os.Stat(posterPath); os.IsNotExist(err) {
		posterPath = filepath.Join("uploads", "posters", fmt.Sprintf("%s.png", mid))
	}
	if _, err := os.Stat(posterPath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "poster image not found"})
		return
	}
	c.Header("Cache-Control", "public, max-age=86400")
	c.File(posterPath)
}

func (s *Server) CreateMatch(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	if !mustAdmin(s.db, gid, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	var in struct {
		Name              string `json:"name"`
		ScheduledAt       string `json:"scheduled_at"`
		VenueID           string `json:"venue_id"`
		Venue             string `json:"venue"`
		VenueMapURL       string `json:"venue_map_url"`
		Format            string `json:"format"`
		Notes             string `json:"notes"`
		DurationMinutes   int    `json:"duration_minutes"`
		TeamCount         int    `json:"team_count"`
		PlayersPerTeam    int    `json:"players_per_team"`
		MaxPlayers        int    `json:"max_players"`
		PollID            string `json:"poll_id"`
		TournamentType    string `json:"tournament_type"`
		MiniMatchDuration int    `json:"mini_match_duration"`
		DrawRule          string `json:"draw_rule"`
		TotalMiniMatches  int    `json:"total_mini_matches"`
		BreakMinutes      int    `json:"break_minutes"`
	}
	if c.BindJSON(&in) != nil || in.Name == "" {
		c.JSON(400, err("match name required"))
		return
	}
	var g models.Group
	if e := s.db.First(&g, gid).Error; e != nil {
		c.JSON(404, err("group not found"))
		return
	}
	t, e := time.Parse(time.RFC3339, in.ScheduledAt)
	if e != nil {
		t = time.Now().Add(24 * time.Hour)
	}

	actualTeamCount := defaultInt(in.TeamCount, 2)
	tournamentType := in.TournamentType
	miniMatchDuration := in.MiniMatchDuration
	drawRule := in.DrawRule
	totalMiniMatches := in.TotalMiniMatches
	breakMinutes := in.BreakMinutes

	// Strict requirement: 2 teams remain identical to before (pure single match)
	if actualTeamCount <= 2 {
		tournamentType = "NONE"
		miniMatchDuration = 0
		drawRule = ""
		totalMiniMatches = 0
		breakMinutes = 0
	} else {
		if tournamentType == "" || tournamentType == "NONE" {
			tournamentType = "WINNER_STAYS"
		}
		if miniMatchDuration <= 0 {
			miniMatchDuration = 15
		}
		if drawRule == "" {
			drawRule = "DEFENDER_STAYS"
		}
		if breakMinutes <= 0 {
			breakMinutes = 5
		}
		if totalMiniMatches <= 0 {
			totalMiniMatches = CalculateTotalMiniMatches(in.DurationMinutes, miniMatchDuration, breakMinutes)
		}
	}

	venueName, mapURL := resolveVenue(in.Venue, in.VenueMapURL)
	m := models.Match{
		GroupID:           gid,
		SportID:           g.SportID,
		Name:              in.Name,
		ScheduledAt:       t,
		Format:            in.Format,
		DurationMinutes:   in.DurationMinutes,
		TeamCount:         actualTeamCount,
		PlayersPerTeam:    in.PlayersPerTeam,
		MaxPlayers:        in.MaxPlayers,
		Notes:             in.Notes,
		Venue:             venueName,
		VenueMapURL:       mapURL,
		Status:            "UPCOMING",
		TournamentType:    tournamentType,
		MiniMatchDuration: miniMatchDuration,
		DrawRule:          drawRule,
		TotalMiniMatches:  totalMiniMatches,
		BreakMinutes:      breakMinutes,
	}
	if in.VenueID != "" {
		id := mustUUID(in.VenueID)
		m.VenueID = &id
		if m.Venue == "" || m.VenueMapURL == "" {
			var v models.Venue
			if s.db.First(&v, id).Error == nil {
				if m.Venue == "" {
					m.Venue = v.Name
				}
				if m.VenueMapURL == "" {
					m.VenueMapURL = v.GoogleMapsURL
				}
			}
		}
	}
	if e = s.db.Create(&m).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}

	// If match was created from an availability poll, auto-RSVP all IN voters and delete the poll
	if in.PollID != "" {
		pollID := mustUUID(in.PollID)
		if pollID != uuid.Nil {
			var inVotes []models.PollVote
			if s.db.Where("poll_id = ? AND option = 'IN'", pollID).Find(&inVotes).Error == nil && len(inVotes) > 0 {
				for _, v := range inVotes {
					att := models.Attendance{
						MatchID:     m.ID,
						UserID:      v.UserID,
						Status:      "GOING",
						RespondedAt: time.Now(),
					}
					s.db.Clauses(clause.OnConflict{
						Columns:   []clause.Column{{Name: "match_id"}, {Name: "user_id"}},
						DoUpdates: clause.AssignmentColumns([]string{"status", "responded_at"}),
					}).Create(&att)
				}
			}
			s.db.Where("poll_id = ?", pollID).Delete(&models.PollVote{})
			s.db.Where("id = ?", pollID).Delete(&models.Poll{})
		}
	}

	schedulerID := mustUUID(auth.UserID(c))
	notifyGroupMembers(s.db, gid, &schedulerID, "MATCH_SCHEDULED", "New Match Scheduled", fmt.Sprintf("'%s' has been scheduled for %s in %s. Check the lineup and RSVP!", m.Name, m.ScheduledAt.Format("Mon, Jan 02 • 15:04"), g.Name), "MATCH", &m.ID)
	c.JSON(201, gin.H{"success": true, "data": m})
}

func (s *Server) ListGroupMatches(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	var msx []models.Match
	s.db.Where("group_id=?", gid).Order("scheduled_at DESC").Find(&msx)
	for i := range msx {
		if msx[i].VenueID != nil && msx[i].Venue == "" {
			var v models.Venue
			if s.db.First(&v, *msx[i].VenueID).Error == nil {
				msx[i].Venue = v.Name
				if msx[i].VenueMapURL == "" {
					msx[i].VenueMapURL = v.GoogleMapsURL
				}
			}
		}
	}
	c.JSON(200, gin.H{"success": true, "data": msx})
}

func (s *Server) MatchMembershipMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.FullPath(), "/api/matches/") {
			c.Next()
			return
		}
		id, e := uuid.Parse(c.Param("id"))
		if e != nil {
			c.AbortWithStatusJSON(400, err("invalid match id"))
			return
		}
		var m models.Match
		cacheKey := "squadup:cache:match:" + id.String()
		found, _ := s.cache.Get(c.Request.Context(), cacheKey, &m)
		if !found {
			if e := s.db.First(&m, id).Error; e != nil {
				if e == gorm.ErrRecordNotFound {
					c.AbortWithStatusJSON(404, err("match not found"))
				} else {
					c.AbortWithStatusJSON(500, err("could not load match"))
				}
				return
			}
			if m.VenueID != nil && m.Venue == "" {
				var v models.Venue
				if s.db.First(&v, *m.VenueID).Error == nil {
					m.Venue = v.Name
					if m.VenueMapURL == "" {
						m.VenueMapURL = v.GoogleMapsURL
					}
				}
			}
			_ = s.cache.Set(c.Request.Context(), cacheKey, m, 30*time.Second)
		} else if m.VenueID != nil && m.Venue == "" {
			var v models.Venue
			if s.db.First(&v, *m.VenueID).Error == nil {
				m.Venue = v.Name
				if m.VenueMapURL == "" {
					m.VenueMapURL = v.GoogleMapsURL
				}
			}
		}
		if !mustMember(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
			c.AbortWithStatusJSON(403, err("not a group member"))
			return
		}
		c.Set("match", m)
		c.Next()
	}
}

func (s *Server) GetMatch(c *gin.Context) {
	c.JSON(200, gin.H{"success": true, "data": c.MustGet("match")})
}

func (s *Server) UpdateMatch(c *gin.Context) {
	m := c.MustGet("match").(models.Match)
	uid := mustUUID(auth.UserID(c))
	if !mustAdmin(s.db, m.GroupID, uid) {
		c.JSON(403, err("admin only"))
		return
	}
	if m.Status != "UPCOMING" {
		c.JSON(400, err("only upcoming matches can be edited"))
		return
	}
	var in struct {
		Name              string `json:"name"`
		ScheduledAt       string `json:"scheduled_at"`
		VenueID           string `json:"venue_id"`
		Venue             string `json:"venue"`
		VenueMapURL       string `json:"venue_map_url"`
		Format            string `json:"format"`
		Notes             string `json:"notes"`
		DurationMinutes   int    `json:"duration_minutes"`
		TeamCount         int    `json:"team_count"`
		PlayersPerTeam    int    `json:"players_per_team"`
		MaxPlayers        int    `json:"max_players"`
		TournamentType    string `json:"tournament_type"`
		MiniMatchDuration int    `json:"mini_match_duration"`
		DrawRule          string `json:"draw_rule"`
		TotalMiniMatches  int    `json:"total_mini_matches"`
		BreakMinutes      int    `json:"break_minutes"`
	}
	if c.BindJSON(&in) != nil || strings.TrimSpace(in.Name) == "" {
		c.JSON(400, err("match name required"))
		return
	}
	if in.ScheduledAt != "" {
		if t, errTime := time.Parse(time.RFC3339, in.ScheduledAt); errTime == nil {
			m.ScheduledAt = t
		}
	}
	m.Name = strings.TrimSpace(in.Name)
	if in.Format != "" {
		m.Format = in.Format
	}
	if in.DurationMinutes > 0 {
		m.DurationMinutes = in.DurationMinutes
	}
	if in.TeamCount > 0 {
		m.TeamCount = in.TeamCount
	}
	if in.PlayersPerTeam > 0 {
		m.PlayersPerTeam = in.PlayersPerTeam
	}
	if in.MaxPlayers > 0 {
		m.MaxPlayers = in.MaxPlayers
	} else if m.TeamCount > 0 && m.PlayersPerTeam > 0 {
		m.MaxPlayers = m.TeamCount * m.PlayersPerTeam
	}

	// 2 teams rule: strictly disable tournament fields if <= 2 teams
	if m.TeamCount <= 2 {
		m.TournamentType = "NONE"
		m.MiniMatchDuration = 0
		m.DrawRule = ""
		m.TotalMiniMatches = 0
		m.BreakMinutes = 0
	} else {
		if in.TournamentType != "" {
			m.TournamentType = in.TournamentType
		}
		if in.MiniMatchDuration > 0 {
			m.MiniMatchDuration = in.MiniMatchDuration
		}
		if in.DrawRule != "" {
			m.DrawRule = in.DrawRule
		}
		if in.BreakMinutes > 0 {
			m.BreakMinutes = in.BreakMinutes
		} else if m.BreakMinutes <= 0 {
			m.BreakMinutes = 5
		}
		if in.TotalMiniMatches > 0 {
			m.TotalMiniMatches = in.TotalMiniMatches
		} else {
			m.TotalMiniMatches = CalculateTotalMiniMatches(m.DurationMinutes, m.MiniMatchDuration, m.BreakMinutes)
		}
	}

	m.Notes = in.Notes
	venueName, mapURL := resolveVenue(in.Venue, in.VenueMapURL)
	m.Venue = venueName
	m.VenueMapURL = mapURL
	if in.VenueID != "" {
		vid := mustUUID(in.VenueID)
		m.VenueID = &vid
		if m.Venue == "" || m.VenueMapURL == "" {
			var v models.Venue
			if s.db.First(&v, vid).Error == nil {
				if m.Venue == "" {
					m.Venue = v.Name
				}
				if m.VenueMapURL == "" {
					m.VenueMapURL = v.GoogleMapsURL
				}
			}
		}
	} else {
		m.VenueID = nil
	}
	if e := s.db.Save(&m).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+m.ID.String())
	c.JSON(200, gin.H{"success": true, "data": m})
}

func (s *Server) DeleteMatch(c *gin.Context) {
	m := c.MustGet("match").(models.Match)
	uid := mustUUID(auth.UserID(c))
	if !mustAdmin(s.db, m.GroupID, uid) {
		c.JSON(403, err("admin only"))
		return
	}
	if m.Status != "UPCOMING" {
		c.JSON(400, err("only upcoming matches can be deleted"))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+m.ID.String(), "squadup:cache:match:"+m.ID.String()+":events")
	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		var teams []models.Team
		if errVal := tx.Where("match_id = ?", m.ID).Find(&teams).Error; errVal != nil {
			return errVal
		}
		teamIDs := make([]uuid.UUID, 0, len(teams))
		for _, t := range teams {
			teamIDs = append(teamIDs, t.ID)
		}
		if len(teamIDs) > 0 {
			if errVal := tx.Where("team_id IN ?", teamIDs).Delete(&models.TeamMember{}).Error; errVal != nil {
				return errVal
			}
			if errVal := tx.Where("id IN ?", teamIDs).Delete(&models.Team{}).Error; errVal != nil {
				return errVal
			}
		}
		if errVal := tx.Where("match_id = ?", m.ID).Delete(&models.Attendance{}).Error; errVal != nil {
			return errVal
		}
		if errVal := tx.Where("match_id = ?", m.ID).Delete(&models.MatchEvent{}).Error; errVal != nil {
			return errVal
		}
		if errVal := tx.Where("match_id = ?", m.ID).Delete(&models.MatchResult{}).Error; errVal != nil {
			return errVal
		}
		return tx.Delete(&m).Error
	})
	if txErr != nil {
		c.JSON(500, err("could not delete match"))
		return
	}
	c.JSON(200, gin.H{"success": true, "message": "match deleted successfully"})
}

func (s *Server) StartMatch(c *gin.Context) {
	var m models.Match
	s.db.First(&m, mustUUID(c.Param("id")))
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	o, e := s.matchService.Start(m.ID)
	if e != nil {
		c.JSON(400, err(e.Error()))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+m.ID.String())
	var g models.Group
	s.db.First(&g, m.GroupID)
	notifyGroupMembers(s.db, m.GroupID, nil, "MATCH_LIVE", "Match is LIVE! ⚽", fmt.Sprintf("'%s' has kicked off in %s! Follow the live scoreline.", m.Name, g.Name), "MATCH", &m.ID)
	c.JSON(200, gin.H{"success": true, "data": o})
}

func (s *Server) PauseMatch(c *gin.Context) {
	var m models.Match
	if errVal := s.db.First(&m, mustUUID(c.Param("id"))).Error; errVal != nil {
		c.JSON(404, err("match not found"))
		return
	}
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	o, e := s.matchService.Pause(m.ID)
	if e != nil {
		c.JSON(400, err(e.Error()))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+m.ID.String())
	c.JSON(200, gin.H{"success": true, "data": o})
}

func (s *Server) ResumeMatch(c *gin.Context) {
	var m models.Match
	if errVal := s.db.First(&m, mustUUID(c.Param("id"))).Error; errVal != nil {
		c.JSON(404, err("match not found"))
		return
	}
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	o, e := s.matchService.Resume(m.ID)
	if e != nil {
		c.JSON(400, err(e.Error()))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+m.ID.String())
	c.JSON(200, gin.H{"success": true, "data": o})
}

func (s *Server) FinishMatch(c *gin.Context) {
	var m models.Match
	s.db.First(&m, mustUUID(c.Param("id")))
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	o, e := s.matchService.Finish(m.ID)
	if e != nil {
		c.JSON(400, err(e.Error()))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+m.ID.String())
	c.JSON(200, gin.H{"success": true, "data": o})
}

func (s *Server) AddMatchEvent(c *gin.Context) {
	var m models.Match
	s.db.First(&m, mustUUID(c.Param("id")))
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	var in struct {
		TeamID           *uuid.UUID `json:"team_id"`
		PlayerID         *uuid.UUID `json:"player_id"`
		AssistPlayerID   *uuid.UUID `json:"assist_player_id"`
		EventType        string     `json:"event_type"`
		MatchTimeSeconds int        `json:"match_time_seconds"`
		Metadata         string     `json:"metadata"`
	}
	if c.ShouldBindJSON(&in) != nil || in.EventType == "" {
		c.JSON(400, err("event type required"))
		return
	}
	ev := models.MatchEvent{
		EventType:        in.EventType,
		MatchTimeSeconds: in.MatchTimeSeconds,
		Metadata:         in.Metadata,
		TeamID:           in.TeamID,
		PlayerID:         in.PlayerID,
		AssistPlayerID:   in.AssistPlayerID,
	}
	if e := s.matchService.AddEvent(m.ID, &ev); e != nil {
		c.JSON(400, err(e.Error()))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+m.ID.String()+":events")
	c.JSON(201, gin.H{"success": true, "data": ev})
}

func (s *Server) DeleteMatchEvent(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	eid := mustUUID(c.Param("eventId"))
	var m models.Match
	if e := s.db.First(&m, mid).Error; e != nil {
		c.JSON(404, err("match not found"))
		return
	}
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	if e := s.matchService.DeleteEvent(mid, eid); e != nil {
		c.JSON(400, err(e.Error()))
		return
	}
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+mid.String()+":events")
	c.JSON(200, gin.H{"success": true})
}

func (s *Server) GetMatchEvents(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	cacheKey := "squadup:cache:match:" + mid.String() + ":events"
	var es []models.MatchEvent
	if ok, _ := s.cache.Get(c.Request.Context(), cacheKey, &es); ok {
		c.JSON(200, gin.H{"success": true, "data": es})
		return
	}
	if e := s.db.Where("match_id=?", mid).Order("match_time_seconds ASC,created_at ASC, id ASC").Find(&es).Error; e != nil {
		c.JSON(500, err("could not load events"))
		return
	}
	_ = s.cache.Set(c.Request.Context(), cacheKey, es, 30*time.Second)
	c.JSON(200, gin.H{"success": true, "data": es})
}

func (s *Server) GetMatchResult(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	var result models.MatchResult
	if e := s.db.Where("match_id=?", mid).First(&result).Error; e != nil {
		c.JSON(404, err("match result not found"))
		return
	}
	c.JSON(200, gin.H{"success": true, "data": result})
}

func (s *Server) FinalizeMatch(c *gin.Context) {
	mid := mustUUID(c.Param("id"))

	token, acquired, lockErr := s.locker.Acquire(c.Request.Context(), "match:finalize:"+mid.String(), 15*time.Second)
	if lockErr != nil {
		c.JSON(500, err("failed to acquire match lock"))
		return
	}
	if !acquired {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": gin.H{"message": "match finalization is already in progress"}})
		return
	}
	defer s.locker.Release(context.Background(), "match:finalize:"+mid.String(), token)

	var m models.Match
	if e := s.db.First(&m, mid).Error; e != nil {
		c.JSON(404, err("match not found"))
		return
	}
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	var in struct {
		HomeScore int        `json:"home_score"`
		AwayScore int        `json:"away_score"`
		MVPUserID *uuid.UUID `json:"mvp_user_id"`
		Notes     string     `json:"notes"`
	}
	if c.ShouldBindJSON(&in) != nil {
		c.JSON(400, err("invalid request"))
		return
	}
	r0 := models.MatchResult{HomeScore: in.HomeScore, AwayScore: in.AwayScore, MVPUserID: in.MVPUserID, Notes: in.Notes}
	if e := s.matchService.Finalize(mid, &r0); e != nil {
		c.JSON(400, err(e.Error()))
		return
	}

	// Invalidate match cache, events cache, and squad leaderboard cache
	_ = s.cache.Delete(
		c.Request.Context(),
		"squadup:cache:match:"+mid.String(),
		"squadup:cache:match:"+mid.String()+":events",
		"squadup:cache:leaderboard:"+m.GroupID.String(),
	)

	var g models.Group
	s.db.First(&g, m.GroupID)
	notifyGroupMembers(s.db, m.GroupID, nil, "MATCH_FINALIZED", "Match Result Finalized 🏆", fmt.Sprintf("Final score recorded for '%s': %d - %d in %s. Check MVP & squad stats!", m.Name, in.HomeScore, in.AwayScore, g.Name), "MATCH", &mid)
	c.JSON(200, gin.H{"success": true, "data": r0})
}
