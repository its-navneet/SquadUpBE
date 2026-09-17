package match

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"squadup/backend/internal/models"
	"squadup/backend/internal/ws"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrInvalidState = errors.New("invalid match state")

type Service struct {
	DB  *gorm.DB
	Hub *ws.Hub
}

func New(db *gorm.DB, hub *ws.Hub) *Service { return &Service{db, hub} }
func (s *Service) Start(id uuid.UUID) (models.Match, error) {
	return s.transition(id, "LIVE")
}

func (s *Service) transition(id uuid.UUID, status string) (models.Match, error) {
	var m models.Match
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, id).Error; err != nil {
			return err
		}
		if m.Status == status {
			return nil
		} // Retrying must not reset the clock.
		now := time.Now()
		if status == "LIVE" {
			if m.Status != "UPCOMING" {
				return ErrInvalidState
			}
			var count int64
			if err := tx.Model(&models.Team{}).Where("match_id = ?", id).Count(&count).Error; err != nil {
				return err
			}
			if count < 2 {
				return errors.New("generate at least two teams before starting")
			}
			m.StartedAt = &now
		} else {
			if m.Status != "LIVE" {
				return ErrInvalidState
			}
			m.EndedAt = &now
		}
		m.Status = status
		return tx.Save(&m).Error
	})
	if err == nil {
		s.broadcast(id, "MATCH_STATUS_CHANGED", m)
	}
	return m, err
}

func (s *Service) broadcast(id uuid.UUID, kind string, data any) {
	if s.Hub != nil {
		s.Hub.Broadcast(id.String(), ws.Event{Type: kind, Data: data})
	}
}
func (s *Service) Finish(id uuid.UUID) (models.Match, error) {
	return s.transition(id, "COMPLETED")
}

// AddEvent returns the persisted identity to both REST and WebSocket clients.
func (s *Service) AddEvent(id uuid.UUID, e *models.MatchEvent) error {
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var m models.Match
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, id).Error; err != nil {
			return err
		}
		if m.Status != "LIVE" {
			return ErrInvalidState
		}
		if err := validateEvent(tx, id, e); err != nil {
			return err
		}
		e.MatchID = id
		return tx.Create(e).Error
	})
	if err == nil {
		s.broadcast(id, "MATCH_EVENT_CREATED", *e)
	}
	return err
}

func (s *Service) DeleteEvent(matchID, eventID uuid.UUID) error {
	var ev models.MatchEvent
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var m models.Match
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, matchID).Error; err != nil {
			return err
		}
		if m.Status != "LIVE" {
			return ErrInvalidState
		}
		if err := tx.Where("id = ? AND match_id = ?", eventID, matchID).First(&ev).Error; err != nil {
			return err
		}
		return tx.Delete(&ev).Error
	})
	if err == nil {
		s.broadcast(matchID, "MATCH_EVENT_DELETED", map[string]string{"id": eventID.String()})
	}
	return err
}

// Metadata remains JSON text for compatibility with the existing jsonb model.
type EventMetadata struct {
	Note             string     `json:"note,omitempty"`
	IncomingPlayerID *uuid.UUID `json:"incoming_player_id,omitempty"`
	CreditedTeamID   *uuid.UUID `json:"credited_team_id,omitempty"`
	FouledPlayerID   *uuid.UUID `json:"fouled_player_id,omitempty"`
	FoulType         string     `json:"foul_type,omitempty"`
	SaveType         string     `json:"save_type,omitempty"`
}

func validateEvent(tx *gorm.DB, id uuid.UUID, e *models.MatchEvent) error {
	e.EventType = strings.ToUpper(strings.TrimSpace(e.EventType))
	if e.MatchTimeSeconds < 0 || e.MatchTimeSeconds > 86400 {
		return errors.New("invalid match time")
	}
	if e.Metadata == "" {
		e.Metadata = "{}"
	}
	var meta EventMetadata
	if err := json.Unmarshal([]byte(e.Metadata), &meta); err != nil {
		return errors.New("invalid event metadata")
	}
	switch e.EventType {
	case "GOAL", "OWN_GOAL", "YELLOW_CARD", "RED_CARD", "SUBSTITUTION", "SAVE", "FOUL":
		if e.TeamID == nil || e.PlayerID == nil {
			return errors.New("event requires a team and player")
		}
	case "NOTE":
		if strings.TrimSpace(meta.Note) == "" || len(meta.Note) > 2000 {
			return errors.New("note must contain 1 to 2000 characters")
		}
	default:
		return errors.New("unsupported event type")
	}
	if e.TeamID != nil {
		var count int64
		if err := tx.Model(&models.Team{}).Where("id = ? AND match_id = ?", *e.TeamID, id).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return errors.New("team does not belong to match")
		}
	}
	for _, player := range []*uuid.UUID{e.PlayerID, e.AssistPlayerID} {
		if player == nil {
			continue
		}
		if e.TeamID == nil {
			return errors.New("player requires a team")
		}
		var count int64
		if err := tx.Model(&models.TeamMember{}).Where("team_id = ? AND user_id = ?", *e.TeamID, *player).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return errors.New("player does not belong to selected team")
		}
	}
	if e.AssistPlayerID != nil && (e.EventType != "GOAL" || *e.AssistPlayerID == *e.PlayerID) {
		return errors.New("invalid assist player")
	}
	if e.EventType == "FOUL" && meta.FouledPlayerID != nil && e.PlayerID != nil {
		if *meta.FouledPlayerID == *e.PlayerID {
			return errors.New("fouling player and fouled player must be distinct")
		}
	}
	if e.EventType == "SUBSTITUTION" {
		if meta.IncomingPlayerID == nil || *meta.IncomingPlayerID == *e.PlayerID {
			return errors.New("select distinct outgoing and incoming players")
		}
		var inTeamCount int64
		if err := tx.Model(&models.TeamMember{}).Where("team_id = ? AND user_id = ?", *e.TeamID, *meta.IncomingPlayerID).Count(&inTeamCount).Error; err != nil {
			return err
		}
		if inTeamCount == 0 {
			var opposingCount int64
			if err := tx.Model(&models.TeamMember{}).
				Joins("JOIN teams ON teams.id = team_members.team_id").
				Where("teams.match_id = ? AND teams.id <> ? AND team_members.user_id = ?", id, *e.TeamID, *meta.IncomingPlayerID).
				Count(&opposingCount).Error; err != nil {
				return err
			}
			if opposingCount > 0 {
				return errors.New("incoming player belongs to opposing team")
			}
			newMember := models.TeamMember{
				TeamID: *e.TeamID,
				UserID: *meta.IncomingPlayerID,
			}
			if err := tx.Create(&newMember).Error; err != nil {
				return err
			}
		}
	} else if meta.IncomingPlayerID != nil {
		return errors.New("incoming player is only valid for substitutions")
	}
	if e.EventType == "OWN_GOAL" {
		var teams []models.Team
		if err := tx.Where("match_id = ? AND id <> ?", id, *e.TeamID).Find(&teams).Error; err != nil {
			return err
		}
		if meta.CreditedTeamID == nil && len(teams) == 1 {
			meta.CreditedTeamID = &teams[0].ID
		}
		valid := false
		for _, t := range teams {
			if meta.CreditedTeamID != nil && t.ID == *meta.CreditedTeamID {
				valid = true
			}
		}
		if !valid {
			return errors.New("select the opposing team credited with the own goal")
		}
	} else if meta.CreditedTeamID != nil {
		return errors.New("credited team is only valid for own goals")
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	e.Metadata = string(raw)
	return nil
}

func (s *Service) Finalize(id uuid.UUID, result *models.MatchResult) error {
	var m models.Match
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, id).Error; err != nil {
			return err
		}
		if m.Status != "COMPLETED" {
			return ErrInvalidState
		}
		if result.HomeScore < 0 || result.AwayScore < 0 {
			return errors.New("scores cannot be negative")
		}
		if result.MVPUserID != nil {
			var count int64
			if err := tx.Model(&models.TeamMember{}).Joins("JOIN teams ON teams.id = team_members.team_id").Where("teams.match_id = ? AND team_members.user_id = ?", id, *result.MVPUserID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return errors.New("MVP must be a match player")
			}
		}
		var existing models.MatchResult
		err := tx.Where("match_id = ?", id).First(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			result.Base = existing.Base
		}
		result.MatchID = id
		if err := tx.Save(result).Error; err != nil {
			return err
		}
		now := time.Now()
		m.FinalizedAt = &now
		if err := tx.Save(&m).Error; err != nil {
			return err
		}
		return recalculateGroupStats(tx, m.GroupID)
	})
	if err == nil {
		s.broadcast(id, "MATCH_STATUS_CHANGED", m)
	}
	return err
}

// RecalculateGroupStats recalculates player_statistics for all finalized matches in groupID.
func RecalculateGroupStats(tx *gorm.DB, groupID uuid.UUID) error {
	return recalculateGroupStats(tx, groupID)
}

func recalculateGroupStats(tx *gorm.DB, groupID uuid.UUID) error {
	var matches []models.Match
	if err := tx.Where("group_id = ? AND finalized_at IS NOT NULL", groupID).Find(&matches).Error; err != nil {
		return err
	}

	// 1. Preserve or compute accurate player attendance counts
	existingAttendance := make(map[uuid.UUID]struct{ Count, Total int })
	var existingStats []models.PlayerStatistics
	if err := tx.Where("group_id = ?", groupID).Find(&existingStats).Error; err == nil {
		for _, es := range existingStats {
			existingAttendance[es.UserID] = struct{ Count, Total int }{
				Count: es.AttendanceCount,
				Total: es.AttendanceTotal,
			}
		}
	}
	type attAgg struct {
		UserID uuid.UUID `gorm:"column:user_id"`
		Going  int       `gorm:"column:going"`
		Total  int       `gorm:"column:total"`
	}
	var atts []attAgg
	_ = tx.Raw(`
		SELECT a.user_id, 
		       COUNT(CASE WHEN a.status = 'GOING' THEN 1 END) as going,
		       COUNT(*) as total
		FROM attendances a
		INNER JOIN matches m ON a.match_id = m.id
		WHERE m.group_id = ?
		GROUP BY a.user_id
	`, groupID).Scan(&atts).Error
	for _, at := range atts {
		existingAttendance[at.UserID] = struct{ Count, Total int }{
			Count: at.Going,
			Total: at.Total,
		}
	}

	statsMap := make(map[uuid.UUID]*models.PlayerStatistics)
	getStat := func(uid uuid.UUID) *models.PlayerStatistics {
		if st, ok := statsMap[uid]; ok {
			return st
		}
		att := existingAttendance[uid]
		st := &models.PlayerStatistics{
			GroupID:         groupID,
			UserID:          uid,
			AttendanceCount: att.Count,
			AttendanceTotal: att.Total,
		}
		statsMap[uid] = st
		return st
	}

	// Ensure all members with attendance records have a statistics entry
	for uid, att := range existingAttendance {
		st := getStat(uid)
		st.AttendanceCount = att.Count
		st.AttendanceTotal = att.Total
	}

	for _, m := range matches {
		var res models.MatchResult
		if err := tx.Where("match_id = ?", m.ID).First(&res).Error; err != nil {
			continue
		}
		if res.MVPUserID != nil {
			getStat(*res.MVPUserID).MVP++
		}

		var events []models.MatchEvent
		if err := tx.Where("match_id = ?", m.ID).Find(&events).Error; err != nil {
			continue
		}
		for _, ev := range events {
			switch ev.EventType {
			case "GOAL":
				if ev.PlayerID != nil {
					getStat(*ev.PlayerID).Goals++
				}
				if ev.AssistPlayerID != nil {
					getStat(*ev.AssistPlayerID).Assists++
				}
			case "YELLOW_CARD":
				if ev.PlayerID != nil {
					getStat(*ev.PlayerID).YellowCards++
				}
			case "RED_CARD":
				if ev.PlayerID != nil {
					getStat(*ev.PlayerID).RedCards++
				}
			case "SAVE":
				if ev.PlayerID != nil {
					getStat(*ev.PlayerID).Saves++
				}
			case "FOUL":
				if ev.PlayerID != nil {
					getStat(*ev.PlayerID).Fouls++
				}
			}
		}

		var teams []models.Team
		if err := tx.Where("match_id = ?", m.ID).Order("created_at ASC, id ASC").Find(&teams).Error; err != nil {
			continue
		}
		if len(teams) == 2 {
			var t0Members, t1Members []models.TeamMember
			tx.Where("team_id = ?", teams[0].ID).Find(&t0Members)
			tx.Where("team_id = ?", teams[1].ID).Find(&t1Members)

			for _, mem := range t0Members {
				st := getStat(mem.UserID)
				st.Matches++
				if res.HomeScore > res.AwayScore {
					st.Wins++
				} else if res.HomeScore < res.AwayScore {
					st.Losses++
				} else {
					st.Draws++
				}
				if res.AwayScore == 0 {
					st.CleanSheets++
				}
			}

			for _, mem := range t1Members {
				st := getStat(mem.UserID)
				st.Matches++
				if res.AwayScore > res.HomeScore {
					st.Wins++
				} else if res.AwayScore < res.HomeScore {
					st.Losses++
				} else {
					st.Draws++
				}
				if res.HomeScore == 0 {
					st.CleanSheets++
				}
			}
		} else if len(teams) > 2 {
			// Multi-team scoring: rank teams by goals scored in events
			teamScores := make(map[uuid.UUID]int)
			for _, ev := range events {
				if ev.EventType == "GOAL" && ev.TeamID != nil {
					teamScores[*ev.TeamID]++
				}
			}
			maxScore := -1
			for _, t := range teams {
				sc := teamScores[t.ID]
				if sc > maxScore {
					maxScore = sc
				}
			}
			topCount := 0
			for _, t := range teams {
				if teamScores[t.ID] == maxScore {
					topCount++
				}
			}
			for _, t := range teams {
				var members []models.TeamMember
				tx.Where("team_id = ?", t.ID).Find(&members)
				sc := teamScores[t.ID]
				isWinner := sc == maxScore && topCount == 1
				isDraw := sc == maxScore && topCount > 1
				for _, mem := range members {
					st := getStat(mem.UserID)
					st.Matches++
					if isWinner {
						st.Wins++
					} else if isDraw {
						st.Draws++
					} else {
						st.Losses++
					}
				}
			}
		}
	}

	if err := tx.Where("group_id = ?", groupID).Delete(&models.PlayerStatistics{}).Error; err != nil {
		return err
	}
	for _, st := range statsMap {
		if err := tx.Create(st).Error; err != nil {
			return err
		}
	}
	return nil
}
