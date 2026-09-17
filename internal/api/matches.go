package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
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

	// Strict requirement: 2 teams remain identical to before (pure single match)
	if actualTeamCount <= 2 {
		tournamentType = "NONE"
		miniMatchDuration = 0
		drawRule = ""
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
			_ = s.cache.Set(c.Request.Context(), cacheKey, m, 30*time.Second)
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

func (s *Server) GenerateAIPoster(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMatchMember(s.db, mid, uid) {
		c.JSON(403, err("only squad members can generate a match poster"))
		return
	}

	token, acquired, _ := s.locker.Acquire(c.Request.Context(), "match:poster:"+mid.String(), 45*time.Second)
	if !acquired {
		c.JSON(http.StatusTooManyRequests, gin.H{"success": false, "error": gin.H{"message": "AI poster generation is already in progress for this match"}})
		return
	}
	defer s.locker.Release(context.Background(), "match:poster:"+mid.String(), token)

	var in struct {
		APIKey      string `json:"apiKey"`
		Style       string `json:"style"`
		ImageBase64 string `json:"image_base64"`
		PosterURL   string `json:"poster_url"`
	}
	_ = c.ShouldBindJSON(&in)

	var m models.Match
	if s.db.First(&m, mid).Error != nil {
		c.JSON(404, err("match not found"))
		return
	}

	var imgBytes []byte
	var prompt string

	// Check if client uploaded poster directly (e.g. from Firebase AI Logic)
	if f, _, errFile := c.Request.FormFile("image"); errFile == nil {
		defer f.Close()
		imgBytes, _ = io.ReadAll(io.LimitReader(f, 10*1024*1024))
	} else if f, _, errFile := c.Request.FormFile("poster"); errFile == nil {
		defer f.Close()
		imgBytes, _ = io.ReadAll(io.LimitReader(f, 10*1024*1024))
	} else if in.ImageBase64 != "" {
		b64 := in.ImageBase64
		if idx := strings.Index(b64, ","); idx != -1 {
			b64 = b64[idx+1:]
		}
		var decodeErr error
		imgBytes, decodeErr = base64.StdEncoding.DecodeString(b64)
		if decodeErr != nil {
			c.JSON(400, err("invalid base64 image data"))
			return
		}
	}

	// If no pre-generated image bytes provided, generate via server-side Gemini/Google AI
	if len(imgBytes) == 0 {
		apiKey := in.APIKey
		if apiKey == "" && s.imageClient != nil {
			apiKey = s.imageClient.APIKey
		}
		if apiKey == "" {
			apiKey = os.Getenv("GEMINI_API_KEY")
		}
		if apiKey == "" {
			c.JSON(400, err("GEMINI_API_KEY not configured on backend server. Posters can be generated directly using Firebase AI on the app."))
			return
		}

		var ts []models.Team
		s.db.Where("match_id=?", mid).Order("created_at ASC, id ASC").Find(&ts)
		if len(ts) < 2 {
			c.JSON(400, err("Please divide/generate teams first before creating a team division poster."))
			return
		}

		type teamLineup struct {
			Name    string
			Players []string
		}
		var lineups []teamLineup
		for _, t := range ts {
			var tMembers []models.TeamMember
			s.db.Where("team_id = ?", t.ID).Find(&tMembers)
			var uids []uuid.UUID
			for _, tm := range tMembers {
				uids = append(uids, tm.UserID)
			}
			var users []models.User
			if len(uids) > 0 {
				s.db.Where("id IN ?", uids).Find(&users)
			}
			var playerNames []string
			for _, u := range users {
				playerNames = append(playerNames, u.Name)
			}
			lineups = append(lineups, teamLineup{Name: t.Name, Players: playerNames})
		}

		var g models.Group
		s.db.First(&g, m.GroupID)

		var ven models.Venue
		venueName := "Matchday Turf Arena"
		if m.VenueID != nil && s.db.First(&ven, *m.VenueID).Error == nil && ven.Name != "" {
			venueName = ven.Name
		}

		var sp models.Sport
		sportLower := "football"
		if s.db.First(&sp, m.SportID).Error == nil && sp.Name != "" {
			sportLower = strings.ToLower(sp.Name)
		}

		var matchTeamBlocks []string
		for i, line := range lineups {
			tName := line.Name
			if strings.TrimSpace(tName) == "" {
				tName = fmt.Sprintf("Team %d", i+1)
			}
			playersStr := strings.Join(line.Players, ", ")
			if strings.TrimSpace(playersStr) == "" {
				playersStr = "Squad Players"
			}
			matchTeamBlocks = append(matchTeamBlocks, fmt.Sprintf("TEAM %d: %s\nPLAYERS: %s", i+1, tName, playersStr))
		}
		if len(matchTeamBlocks) == 0 {
			matchTeamBlocks = append(matchTeamBlocks, "TEAM 1: Team 1\nPLAYERS: Squad Players", "TEAM 2: Team 2\nPLAYERS: Squad Players")
		}
		matchSection := strings.Join(matchTeamBlocks, "\n\nVS\n\n")

		dateStr := "TBD"
		timeStr := "TBD"
		if !m.ScheduledAt.IsZero() {
			dateStr = m.ScheduledAt.Format("02 Jan 2006")
			timeStr = m.ScheduledAt.Format("03:04 PM")
		}

		teamCount := len(lineups)
		layoutVs := "* Large centered \"VS\"\n"
		if teamCount > 2 {
			layoutVs = fmt.Sprintf("* Multi-team triangular or round-robin division showing all %d teams prominently with VS dividers between them\n", teamCount)
		}

		prompt = fmt.Sprintf(
			"Create a premium modern %s match poster for a casual recreational game.\n\n"+
				"VISUAL STYLE:\n"+
				"- Professional %s sports promotional poster\n"+
				"- Night stadium with dramatic floodlights and subtle fog\n"+
				"- Dark cinematic background with a %s pitch\n"+
				"- Bold modern sports typography\n"+
				"- Clean, minimal, premium graphic design\n"+
				"- Strong contrast and clear visual hierarchy\n"+
				"- Vertical 4:5 social-media poster\n\n"+
				"MATCH INFORMATION:\n"+
				"%s\n\n"+
				"DATE: %s\n"+
				"TIME: %s\n"+
				"VENUE: %s\n\n"+
				"LAYOUT:\n"+
				"- Large 'MATCH DAY' heading at the top\n"+
				"- Display all %d teams prominently\n"+
				"- Display the players underneath their respective teams\n"+
				"%s"+
				"- Display date, time and venue clearly at the bottom\n"+
				"- Keep the layout balanced and uncluttered\n"+
				"- Make all important information readable on a mobile screen\n\n"+
				"TEXT ACCURACY:\n"+
				"- Use the provided team names and player names exactly as written\n"+
				"- Preserve exact spelling, capitalization, numbers and punctuation\n"+
				"- Do not rewrite, abbreviate or modify any provided text\n"+
				"- Do not omit any team or player\n"+
				"- All %d teams must appear in the final poster\n"+
				"- Treat all provided information as fixed text that must be rendered accurately\n\n"+
				"DO NOT ADD:\n"+
				"- No invented players\n"+
				"- No scores\n"+
				"- No sponsors\n"+
				"- No hashtags\n"+
				"- No extra text\n"+
				"- No professional club logos or branding\n"+
				"- No fictional team information\n\n"+
				"Create a polished, premium recreational sports poster with highly accurate typography and a professional modern composition.",
			sportLower,
			sportLower,
			sportLower,
			matchSection,
			dateStr,
			timeStr,
			venueName,
			teamCount,
			layoutVs,
			teamCount,
		)

		var genErr error
		imgBytes, genErr = s.imageClient.Generate(c.Request.Context(), prompt, apiKey)
		if genErr != nil {
			c.JSON(500, err(genErr.Error()))
			return
		}
	}

	mimeType := http.DetectContentType(imgBytes)
	if !strings.HasPrefix(mimeType, "image/") {
		trimmed := strings.TrimSpace(string(imgBytes))
		if strings.HasPrefix(trimmed, "{") {
			c.JSON(500, err(fmt.Sprintf("image generation failed: %s", trimmed)))
			return
		}
		mimeType = "image/png"
	}

	// Persist poster image to storage (Amazon S3 / B2 or fallback)
	posterFilename := fmt.Sprintf("posters/%s.png", mid.String())
	posterURL, uploadErr := s.storage.Upload(c.Request.Context(), posterFilename, imgBytes, "image/png")
	if uploadErr != nil {
		log.Printf("Failed to upload poster to storage: %v", uploadErr)
		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		posterURL = fmt.Sprintf("%s://%s/api/matches/%s/poster-image", scheme, c.Request.Host, mid.String())
	} else if strings.HasPrefix(posterURL, "/") {
		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		posterURL = fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, posterURL)
	}

	// Keep local disk copies as fallback for /api/matches/:id/poster-image
	uploadsDir := filepath.Join("uploads", "match-posters")
	_ = os.MkdirAll(uploadsDir, 0755)
	_ = os.WriteFile(filepath.Join(uploadsDir, fmt.Sprintf("%s.png", mid.String())), imgBytes, 0644)
	uploadsLegacyDir := filepath.Join("uploads", "posters")
	_ = os.MkdirAll(uploadsLegacyDir, 0755)
	_ = os.WriteFile(filepath.Join(uploadsLegacyDir, fmt.Sprintf("%s.png", mid.String())), imgBytes, 0644)

	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(imgBytes))

	// Persist on match and invalidate cache
	s.db.Model(&models.Match{}).Where("id = ?", mid).Update("poster_url", posterURL)
	_ = s.cache.Delete(c.Request.Context(), "squadup:cache:match:"+mid.String())

	c.JSON(200, gin.H{
		"success": true,
		"data": gin.H{
			"image_url":  dataURL,
			"poster_url": posterURL,
			"prompt":     prompt,
		},
	})
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
