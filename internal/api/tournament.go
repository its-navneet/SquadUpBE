package api

import (
	"sort"
	"strings"
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type TeamStanding struct {
	TeamID         uuid.UUID `json:"team_id"`
	TeamName       string    `json:"team_name"`
	TeamCode       string    `json:"team_code"`
	Played         int       `json:"played"`
	Won            int       `json:"won"`
	Drawn          int       `json:"drawn"`
	Lost           int       `json:"lost"`
	GoalsFor       int       `json:"goals_for"`
	GoalsAgainst   int       `json:"goals_against"`
	GoalDifference int       `json:"goal_difference"`
	Points         int       `json:"points"`
}

// EnsureInitialMiniMatch initializes the first mini-match if teams exist and none have been created yet
func (s *Server) ensureInitialMiniMatch(matchID uuid.UUID, tournamentType string, teams []models.Team) *models.MiniMatch {
	if len(teams) < 2 {
		return nil
	}
	var existing models.MiniMatch
	if s.db.Where("match_id = ?", matchID).First(&existing).Error == nil {
		return &existing
	}

	first := models.MiniMatch{
		MatchID:    matchID,
		GameNumber: 1,
		HomeTeamID: teams[0].ID,
		AwayTeamID: teams[1].ID,
		Status:     "LIVE",
	}
	now := time.Now()
	first.StartedAt = &now
	if s.db.Create(&first).Error == nil {
		return &first
	}
	return nil
}

// CalculateStandings computes live tournament points and goal difference
func calculateStandings(teams []models.Team, miniMatches []models.MiniMatch) []TeamStanding {
	standingsMap := make(map[uuid.UUID]*TeamStanding, len(teams))
	for _, t := range teams {
		standingsMap[t.ID] = &TeamStanding{
			TeamID:   t.ID,
			TeamName: t.Name,
			TeamCode: t.Code,
		}
	}

	for _, m := range miniMatches {
		if m.Status != "COMPLETED" {
			continue
		}
		home, hasHome := standingsMap[m.HomeTeamID]
		away, hasAway := standingsMap[m.AwayTeamID]
		if !hasHome || !hasAway {
			continue
		}

		home.Played++
		away.Played++
		home.GoalsFor += m.HomeScore
		home.GoalsAgainst += m.AwayScore
		away.GoalsFor += m.AwayScore
		away.GoalsAgainst += m.HomeScore

		if m.HomeScore > m.AwayScore {
			home.Won++
			home.Points += 3
			away.Lost++
		} else if m.AwayScore > m.HomeScore {
			away.Won++
			away.Points += 3
			home.Lost++
		} else {
			home.Drawn++
			home.Points++
			away.Drawn++
			away.Points++
		}
	}

	out := make([]TeamStanding, 0, len(teams))
	for _, t := range teams {
		s := standingsMap[t.ID]
		s.GoalDifference = s.GoalsFor - s.GoalsAgainst
		out = append(out, *s)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Points != out[j].Points {
			return out[i].Points > out[j].Points
		}
		if out[i].GoalDifference != out[j].GoalDifference {
			return out[i].GoalDifference > out[j].GoalDifference
		}
		return out[i].GoalsFor > out[j].GoalsFor
	})

	return out
}

// GetMiniMatches returns mini-match fixtures and current standings for a match
func (s *Server) GetMiniMatches(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	var m models.Match
	if s.db.First(&m, mid).Error != nil {
		c.JSON(404, err("match not found"))
		return
	}

	var teams []models.Team
	s.db.Where("match_id = ?", mid).Order("created_at ASC, id ASC").Find(&teams)

	// If match has tournament configured and teams exist, ensure first game is created
	if m.TeamCount > 2 && len(teams) >= 2 {
		s.ensureInitialMiniMatch(mid, m.TournamentType, teams)
	}

	var miniMatches []models.MiniMatch
	s.db.Where("match_id = ?", mid).Order("game_number ASC").Find(&miniMatches)

	standings := calculateStandings(teams, miniMatches)

	c.JSON(200, gin.H{
		"success":         true,
		"tournament_type": m.TournamentType,
		"mini_matches":    miniMatches,
		"standings":       standings,
		"teams":           teams,
	})
}

type RotateMiniMatchInput struct {
	MiniMatchID  string `json:"mini_match_id"`
	HomeScore    int    `json:"home_score"`
	AwayScore    int    `json:"away_score"`
	WinnerTeamID string `json:"winner_team_id"`
}

// RotateMiniMatch ends the current mini-match and sets up the next match in rotation
func (s *Server) RotateMiniMatch(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))

	var m models.Match
	if s.db.First(&m, mid).Error != nil {
		c.JSON(404, err("match not found"))
		return
	}

	if !mustAdmin(s.db, m.GroupID, uid) {
		c.JSON(403, err("only owners or admins can control pitch rotation"))
		return
	}

	if m.TeamCount <= 2 {
		c.JSON(400, err("tournament pitch rotation is only available for multi-team matches (3+ teams)"))
		return
	}

	var in RotateMiniMatchInput
	_ = c.BindJSON(&in)

	var teams []models.Team
	if s.db.Where("match_id = ?", mid).Order("created_at ASC, id ASC").Find(&teams).Error != nil || len(teams) < 2 {
		c.JSON(400, err("teams have not been generated yet"))
		return
	}

	// 1. Locate current active mini-match
	var current models.MiniMatch
	if in.MiniMatchID != "" {
		if s.db.Where("id = ? AND match_id = ?", mustUUID(in.MiniMatchID), mid).First(&current).Error != nil {
			c.JSON(404, err("mini-match not found"))
			return
		}
	} else {
		// Find latest uncompleted mini-match, or the latest overall
		if s.db.Where("match_id = ? AND status != 'COMPLETED'", mid).Order("game_number ASC").First(&current).Error != nil {
			if s.db.Where("match_id = ?", mid).Order("game_number DESC").First(&current).Error != nil {
				// Initialize first if none
				initMatch := s.ensureInitialMiniMatch(mid, m.TournamentType, teams)
				if initMatch != nil {
					current = *initMatch
				}
			}
		}
	}

	// 2. Complete current game
	now := time.Now()
	current.HomeScore = in.HomeScore
	current.AwayScore = in.AwayScore
	current.Status = "COMPLETED"
	current.EndedAt = &now

	// Determine winner
	var winnerID *uuid.UUID
	if current.HomeScore > current.AwayScore {
		w := current.HomeTeamID
		winnerID = &w
	} else if current.AwayScore > current.HomeScore {
		w := current.AwayTeamID
		winnerID = &w
	} else {
		// Tie-break
		if in.WinnerTeamID != "" {
			w := mustUUID(in.WinnerTeamID)
			if w != uuid.Nil {
				winnerID = &w
			}
		}
		if winnerID == nil {
			if strings.EqualFold(m.DrawRule, "CHALLENGER_STAYS") {
				w := current.AwayTeamID
				winnerID = &w
			} else {
				// Default DEFENDER_STAYS
				w := current.HomeTeamID
				winnerID = &w
			}
		}
	}
	current.WinnerTeamID = winnerID
	s.db.Save(&current)

	// 3. Determine next pairing
	var nextHome uuid.UUID
	var nextAway uuid.UUID
	nextGameNum := current.GameNumber + 1

	switch strings.ToUpper(m.TournamentType) {
	case "KNOCKOUT":
		// 4 teams knockout
		// Game 1: Semi-final 1 (T0 vs T1)
		// Game 2: Semi-final 2 (T2 vs T3)
		// Game 3: Final (Winner SF1 vs Winner SF2)
		// Game 4: 3rd Place (Loser SF1 vs Loser SF2)
		if len(teams) >= 4 {
			if current.GameNumber == 1 {
				nextHome = teams[2].ID
				nextAway = teams[3].ID
			} else if current.GameNumber == 2 {
				// Final: Winner SF1 vs Winner SF2
				var sf1, sf2 models.MiniMatch
				s.db.Where("match_id = ? AND game_number = 1", mid).First(&sf1)
				s.db.Where("match_id = ? AND game_number = 2", mid).First(&sf2)
				if sf1.WinnerTeamID != nil {
					nextHome = *sf1.WinnerTeamID
				} else {
					nextHome = sf1.HomeTeamID
				}
				if sf2.WinnerTeamID != nil {
					nextAway = *sf2.WinnerTeamID
				} else {
					nextAway = sf2.HomeTeamID
				}
			} else {
				// Already played final
				c.JSON(200, gin.H{
					"success":         true,
					"completed":       true,
					"message":         "Knockout tournament completed!",
					"tournament_type": m.TournamentType,
				})
				return
			}
		} else {
			// Fallback to Winner Stays
			nextHome, nextAway = determineWinnerStaysPairing(teams, current, winnerID)
		}

	case "ROUND_ROBIN":
		// Round robin rotation sequence
		if len(teams) == 3 {
			// Sequence: (0 vs 1), (1 vs 2), (0 vs 2) -> cycle
			pairings := [][2]int{
				{0, 1},
				{1, 2},
				{0, 2},
			}
			idx := (current.GameNumber) % len(pairings)
			nextHome = teams[pairings[idx][0]].ID
			nextAway = teams[pairings[idx][1]].ID
		} else if len(teams) >= 4 {
			// Sequence for 4 teams: (0,1), (2,3), (0,2), (1,3), (0,3), (1,2) -> cycle
			pairings := [][2]int{
				{0, 1},
				{2, 3},
				{0, 2},
				{1, 3},
				{0, 3},
				{1, 2},
			}
			idx := (current.GameNumber) % len(pairings)
			nextHome = teams[pairings[idx][0]].ID
			nextAway = teams[pairings[idx][1]].ID
		} else {
			nextHome, nextAway = determineWinnerStaysPairing(teams, current, winnerID)
		}

	default: // "WINNER_STAYS" (King of the Pitch)
		nextHome, nextAway = determineWinnerStaysPairing(teams, current, winnerID)
	}

	// 4. Create Next MiniMatch
	nextMatch := models.MiniMatch{
		MatchID:    mid,
		GameNumber: nextGameNum,
		HomeTeamID: nextHome,
		AwayTeamID: nextAway,
		Status:     "LIVE",
		StartedAt:  &now,
	}
	s.db.Create(&nextMatch)

	var miniMatches []models.MiniMatch
	s.db.Where("match_id = ?", mid).Order("game_number ASC").Find(&miniMatches)
	standings := calculateStandings(teams, miniMatches)

	c.JSON(200, gin.H{
		"success":         true,
		"current":         current,
		"next":            nextMatch,
		"tournament_type": m.TournamentType,
		"mini_matches":    miniMatches,
		"standings":       standings,
	})
}

func determineWinnerStaysPairing(teams []models.Team, current models.MiniMatch, winnerID *uuid.UUID) (uuid.UUID, uuid.UUID) {
	var kingID uuid.UUID
	if winnerID != nil {
		kingID = *winnerID
	} else {
		kingID = current.HomeTeamID
	}

	// Find the sitting team(s) not playing in current match
	var sittingTeams []uuid.UUID
	for _, t := range teams {
		if t.ID != current.HomeTeamID && t.ID != current.AwayTeamID {
			sittingTeams = append(sittingTeams, t.ID)
		}
	}

	// Challenger is the first sitting team, or the other team if none sitting
	var challengerID uuid.UUID
	if len(sittingTeams) > 0 {
		challengerID = sittingTeams[0]
	} else {
		// Only 2 teams fallback
		if kingID == current.HomeTeamID {
			challengerID = current.AwayTeamID
		} else {
			challengerID = current.HomeTeamID
		}
	}

	return kingID, challengerID
}
