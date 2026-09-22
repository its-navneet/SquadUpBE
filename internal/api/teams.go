package api

import (
	"fmt"
	"strings"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"
	"squadup/backend/internal/team"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func teamName(i int) string {
	names := []string{"Red", "Blue", "Green", "Yellow", "Orange", "Purple"}
	if i < len(names) {
		return names[i]
	}
	return fmt.Sprintf("Team %d", i+1)
}

func teamCode(i int) string {
	codes := []string{"RED", "BLU", "GRN", "YEL", "ORG", "PUR"}
	if i < len(codes) {
		return codes[i]
	}
	return fmt.Sprintf("T%d", i+1)
}

func loadMatchTeams(d *gorm.DB, matchID uuid.UUID) ([]gin.H, error) {
	var ts []models.Team
	if e := d.Where("match_id=?", matchID).Order("created_at ASC, id ASC").Find(&ts).Error; e != nil {
		return nil, e
	}
	type memberOut struct {
		models.TeamMember
		User *models.User `json:"user,omitempty"`
	}
	if len(ts) == 0 {
		return []gin.H{}, nil
	}

	teamIDs := make([]uuid.UUID, len(ts))
	for i, t := range ts {
		teamIDs[i] = t.ID
	}

	var allMembers []models.TeamMember
	d.Where("team_id IN ?", teamIDs).Find(&allMembers)

	userIDs := make([]uuid.UUID, 0, len(allMembers))
	membersByTeam := make(map[uuid.UUID][]models.TeamMember, len(ts))
	for _, m := range allMembers {
		membersByTeam[m.TeamID] = append(membersByTeam[m.TeamID], m)
		userIDs = append(userIDs, m.UserID)
	}

	uMap := make(map[uuid.UUID]models.User)
	if len(userIDs) > 0 {
		var users []models.User
		d.Where("id IN ?", userIDs).Find(&users)
		for _, u := range users {
			uMap[u.ID] = u
		}
	}

	out := make([]gin.H, 0, len(ts))
	for _, t := range ts {
		mem := membersByTeam[t.ID]
		mList := make([]memberOut, 0, len(mem))
		for _, m := range mem {
			var uPtr *models.User
			if u, ok := uMap[m.UserID]; ok {
				uCopy := u
				uPtr = &uCopy
			}
			mList = append(mList, memberOut{m, uPtr})
		}
		out = append(out, gin.H{
			"id":         t.ID,
			"match_id":   t.MatchID,
			"name":       t.Name,
			"code":       t.Code,
			"strength":   t.Strength,
			"created_at": t.CreatedAt,
			"updated_at": t.UpdatedAt,
			"members":    mList,
		})
	}
	return out, nil
}

func (s *Server) recalcTeamStrength(tx *gorm.DB, teamID, groupID uuid.UUID) error {
	var members []models.TeamMember
	if errVal := tx.Where("team_id = ?", teamID).Find(&members).Error; errVal != nil {
		return errVal
	}
	if len(members) == 0 {
		return tx.Model(&models.Team{}).Where("id = ?", teamID).Update("strength", 0.0).Error
	}
	userIDs := make([]uuid.UUID, len(members))
	for i, m := range members {
		userIDs[i] = m.UserID
	}
	ratingsMap, _ := s.ratingService.BatchAverages(groupID, userIDs)
	var totalRating float64
	for _, m := range members {
		r := ratingsMap[m.UserID]
		if r == 0 {
			r = 5.0
		}
		totalRating += r
	}
	strength := totalRating / float64(len(members))
	return tx.Model(&models.Team{}).Where("id = ?", teamID).Update("strength", strength).Error
}

func (s *Server) GenerateTeams(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	var m models.Match
	s.db.First(&m, mid)
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	if m.Status != "UPCOMING" {
		c.JSON(400, err("squads can only be generated for upcoming matches"))
		return
	}

	teamCount := defaultInt(m.TeamCount, 2)

	// 1. Query attendances with status = 'GOING' for this match,
	// ensuring attendees are active members of the group.
	var ax []models.Attendance
	if e := s.db.Joins("JOIN group_members gm ON gm.user_id = attendances.user_id AND gm.group_id = ? AND gm.status = 'ACTIVE'", m.GroupID).
		Where("attendances.match_id = ? AND attendances.status = 'GOING'", mid).
		Order("attendances.responded_at ASC, attendances.created_at ASC").
		Find(&ax).Error; e != nil {
		c.JSON(500, err("could not query match attendance"))
		return
	}

	if len(ax) == 0 {
		c.JSON(400, err("No players have RSVP'd 'GOING' yet. Squad generation requires players with confirmed GOING attendance."))
		return
	}

	if len(ax) < teamCount {
		c.JSON(400, gin.H{"error": fmt.Sprintf("At least %d players with 'GOING' attendance are required to generate squads (currently %d).", teamCount, len(ax))})
		return
	}

	// If match has MaxPlayers set and more players RSVP'd GOING than capacity,
	// only divide the first MaxPlayers who confirmed attendance.
	if m.MaxPlayers > 0 && len(ax) > m.MaxPlayers {
		ax = ax[:m.MaxPlayers]
	}

	ids := make([]uuid.UUID, 0, len(ax))
	for _, a := range ax {
		ids = append(ids, a.UserID)
	}

	var us []models.User
	s.db.Where("id IN ?", ids).Find(&us)
	userMap := make(map[uuid.UUID]models.User, len(us))
	for _, u := range us {
		userMap[u.ID] = u
	}

	ratings, _ := s.ratingService.BatchAverages(m.GroupID, ids)

	ps := make([]team.Player, 0, len(ids))
	for _, uid := range ids {
		u, ok := userMap[uid]
		if !ok {
			continue
		}
		r := ratings[uid]
		if r == 0 {
			r = 5.0
		}
		ps = append(ps, team.Player{
			UserID:     u.ID,
			Name:       u.Name,
			Rating:     r,
			Position:   u.Position,
			Goalkeeper: strings.EqualFold(u.Position, "GK") || strings.EqualFold(u.Position, "GOALKEEPER"),
		})
	}

	if len(ps) < teamCount {
		c.JSON(400, gin.H{"error": fmt.Sprintf("At least %d active players are required to generate squads (currently %d).", teamCount, len(ps))})
		return
	}

	teams := team.Generate(ps, teamCount)
	tx := s.db.Begin()

	if tx.Error != nil {
		c.JSON(500, err(tx.Error.Error()))
		return
	}

	// Serialize regeneration with starting/recording to preserve event team IDs.
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, mid).Error; e != nil {
		tx.Rollback()
		c.JSON(500, err(e.Error()))
		return
	}
	if m.Status != "UPCOMING" {
		tx.Rollback()
		c.JSON(400, err("teams can only be generated before kickoff"))
		return
	}
	// 1. Find existing teams for this match
	var teamIDs []uuid.UUID

	if e := tx.
		Model(&models.Team{}).
		Where("match_id = ?", mid).
		Pluck("id", &teamIDs).Error; e != nil {

		tx.Rollback()
		c.JSON(500, err(e.Error()))
		return
	}

	// 2. Delete members belonging to those teams
	if len(teamIDs) > 0 {

		if e := tx.
			Where("team_id IN ?", teamIDs).
			Delete(&models.TeamMember{}).Error; e != nil {

			tx.Rollback()
			c.JSON(500, err(e.Error()))
			return
		}
	}

	// 3. Delete existing teams
	if e := tx.
		Where("match_id = ?", mid).
		Delete(&models.Team{}).Error; e != nil {

		tx.Rollback()
		c.JSON(500, err(e.Error()))
		return
	}

	// 4. Create newly generated teams
	out := []models.Team{}
	var allNewMembers []models.TeamMember

	for i, p := range teams {
		tm := models.Team{
			MatchID:  mid,
			Name:     teamName(i),
			Code:     teamCode(i),
			Strength: team.Strength(p),
		}

		if e := tx.Create(&tm).Error; e != nil {
			tx.Rollback()
			c.JSON(500, err(e.Error()))
			return
		}

		for _, x := range p {
			allNewMembers = append(allNewMembers, models.TeamMember{
				TeamID:       tm.ID,
				UserID:       x.UserID,
				PositionName: normalizePosition(x.Position),
			})
		}

		out = append(out, tm)
	}

	if len(allNewMembers) > 0 {
		if e := tx.Create(&allNewMembers).Error; e != nil {
			tx.Rollback()
			c.JSON(500, err(e.Error()))
			return
		}
	}

	// 6. Commit
	if e := tx.Commit().Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}

	notifyGroupMembers(s.db, m.GroupID, nil, "SQUADS_GENERATED", "Balanced Squads Generated ⚔️", fmt.Sprintf("Squads have been generated for '%s'. Check your team assignment!", m.Name), "MATCH", &mid)

	c.JSON(200, gin.H{
		"success": true,
		"data":    out,
	})
}

func (s *Server) GetMatchTeams(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	out, errLoad := loadMatchTeams(s.db, mid)
	if errLoad != nil {
		c.JSON(500, err("could not load teams"))
		return
	}
	c.JSON(200, gin.H{"success": true, "data": out})
}

func (s *Server) MovePlayer(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	var m models.Match
	if e := s.db.First(&m, mid).Error; e != nil {
		c.JSON(404, err("match not found"))
		return
	}
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	if m.Status != "UPCOMING" {
		c.JSON(400, err("teams can only be adjusted for upcoming matches"))
		return
	}

	var in struct {
		UserID       string `json:"user_id"`
		TargetTeamID string `json:"target_team_id"`
	}
	if e := c.ShouldBindJSON(&in); e != nil {
		c.JSON(400, err("invalid request body"))
		return
	}
	playerUID, errP := uuid.Parse(in.UserID)
	targetTeamUID, errT := uuid.Parse(in.TargetTeamID)
	if errP != nil || errT != nil {
		c.JSON(400, err("valid user_id and target_team_id required"))
		return
	}

	var targetTeam models.Team
	if e := s.db.Where("id = ? AND match_id = ?", targetTeamUID, mid).First(&targetTeam).Error; e != nil {
		c.JSON(404, err("target team not found for this match"))
		return
	}

	var currentMember models.TeamMember
	if e := s.db.Joins("JOIN teams ON teams.id = team_members.team_id").
		Where("teams.match_id = ? AND team_members.user_id = ?", mid, playerUID).
		First(&currentMember).Error; e != nil {
		c.JSON(404, err("player is not assigned to any team in this match"))
		return
	}

	if currentMember.TeamID == targetTeamUID {
		c.JSON(400, err("player is already in this team"))
		return
	}

	oldTeamID := currentMember.TeamID

	tx := s.db.Begin()
	if tx.Error != nil {
		c.JSON(500, err(tx.Error.Error()))
		return
	}

	if e := tx.Model(&models.TeamMember{}).
		Where("team_id = ? AND user_id = ?", oldTeamID, playerUID).
		Update("team_id", targetTeamUID).Error; e != nil {
		tx.Rollback()
		c.JSON(500, err("could not move player"))
		return
	}

	_ = s.recalcTeamStrength(tx, oldTeamID, m.GroupID)
	_ = s.recalcTeamStrength(tx, targetTeamUID, m.GroupID)

	if e := tx.Commit().Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}

	out, errLoad := loadMatchTeams(s.db, mid)
	if errLoad != nil {
		c.JSON(500, err("could not load updated teams"))
		return
	}

	c.JSON(200, gin.H{
		"success": true,
		"data":    out,
		"message": "Player moved successfully",
	})
}

func (s *Server) SwapPlayers(c *gin.Context) {
	mid := mustUUID(c.Param("id"))
	var m models.Match
	if e := s.db.First(&m, mid).Error; e != nil {
		c.JSON(404, err("match not found"))
		return
	}
	if !mustAdmin(s.db, m.GroupID, mustUUID(auth.UserID(c))) {
		c.JSON(403, err("admin only"))
		return
	}
	if m.Status != "UPCOMING" {
		c.JSON(400, err("teams can only be adjusted for upcoming matches"))
		return
	}

	var in struct {
		Player1UserID string `json:"player1_user_id"`
		Player2UserID string `json:"player2_user_id"`
	}
	if e := c.ShouldBindJSON(&in); e != nil {
		c.JSON(400, err("invalid request body"))
		return
	}
	p1UID, errP1 := uuid.Parse(in.Player1UserID)
	p2UID, errP2 := uuid.Parse(in.Player2UserID)
	if errP1 != nil || errP2 != nil {
		c.JSON(400, err("valid player1_user_id and player2_user_id required"))
		return
	}
	if p1UID == p2UID {
		c.JSON(400, err("cannot swap a player with themselves"))
		return
	}

	var mem1 models.TeamMember
	if e := s.db.Joins("JOIN teams ON teams.id = team_members.team_id").
		Where("teams.match_id = ? AND team_members.user_id = ?", mid, p1UID).
		First(&mem1).Error; e != nil {
		c.JSON(404, err("player 1 is not assigned to any team in this match"))
		return
	}

	var mem2 models.TeamMember
	if e := s.db.Joins("JOIN teams ON teams.id = team_members.team_id").
		Where("teams.match_id = ? AND team_members.user_id = ?", mid, p2UID).
		First(&mem2).Error; e != nil {
		c.JSON(404, err("player 2 is not assigned to any team in this match"))
		return
	}

	if mem1.TeamID == mem2.TeamID {
		c.JSON(400, err("both players are already on the same team"))
		return
	}

	tx := s.db.Begin()
	if tx.Error != nil {
		c.JSON(500, err(tx.Error.Error()))
		return
	}

	team1ID := mem1.TeamID
	team2ID := mem2.TeamID

	if e := tx.Model(&models.TeamMember{}).
		Where("team_id = ? AND user_id = ?", team1ID, p1UID).
		Update("team_id", team2ID).Error; e != nil {
		tx.Rollback()
		c.JSON(500, err("failed swapping player 1"))
		return
	}

	if e := tx.Model(&models.TeamMember{}).
		Where("team_id = ? AND user_id = ?", team2ID, p2UID).
		Update("team_id", team1ID).Error; e != nil {
		tx.Rollback()
		c.JSON(500, err("failed swapping player 2"))
		return
	}

	_ = s.recalcTeamStrength(tx, team1ID, m.GroupID)
	_ = s.recalcTeamStrength(tx, team2ID, m.GroupID)

	if e := tx.Commit().Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}

	out, errLoad := loadMatchTeams(s.db, mid)
	if errLoad != nil {
		c.JSON(500, err("could not load updated teams"))
		return
	}

	c.JSON(200, gin.H{
		"success": true,
		"data":    out,
		"message": "Players swapped successfully",
	})
}
