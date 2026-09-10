package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"squadup/backend/internal/auth"
	"squadup/backend/internal/client"
	"squadup/backend/internal/config"
	"squadup/backend/internal/database"
	"squadup/backend/internal/group"
	"squadup/backend/internal/limiter"
	"squadup/backend/internal/match"
	"squadup/backend/internal/models"
	"squadup/backend/internal/rating"
	"squadup/backend/internal/team"
	"squadup/backend/internal/ws"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func main() {
	cfg := config.Load()
	db := database.Open(cfg.DatabaseURL)
	if e := database.Migrate(db); e != nil {
		log.Fatal(e)
	}
	seed(db)
	as := auth.New(cfg.JWTSecret)
	hub := ws.New()
	gs := group.New(db)
	rs := rating.New(db)
	ms := match.New(db, hub)
	imageClient := client.NewImageClient(
		cfg.ImageGenURL,
		cfg.ImageGenKey,
	)

	var apiLimiter *limiter.Limiter
	var authLimiter *limiter.Limiter
	if cfg.RateLimitEnabled {
		apiLimiter = limiter.New(cfg.RateLimitRPS, cfg.RateLimitBurst, 5*time.Minute, 15*time.Minute)
		defer apiLimiter.Stop()

		authRatePerSec := float64(cfg.AuthRateLimitRPM) / 60.0
		authLimiter = limiter.New(authRatePerSec, cfg.AuthRateLimitBurst, 5*time.Minute, 15*time.Minute)
		defer authLimiter.Stop()
	}

	r := gin.Default()
	r.Use(cors(cfg.CORSOrigins))
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.Static("/uploads", "./uploads")
	api := r.Group("/api")
	if apiLimiter != nil {
		api.Use(apiLimiter.Middleware(limiter.UserOrIPKeyExtractor))
	}

	var authLimitMiddleware gin.HandlerFunc
	if authLimiter != nil {
		authLimitMiddleware = authLimiter.Middleware(limiter.IPKeyExtractor)
	} else {
		authLimitMiddleware = func(c *gin.Context) { c.Next() }
	}

	api.GET("/matches/:id/poster-image", func(c *gin.Context) {
		mid := c.Param("id")
		filePath := filepath.Join("uploads", "posters", fmt.Sprintf("%s.png", mid))
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "poster image not found"})
			return
		}
		c.Header("Cache-Control", "public, max-age=86400")
		c.File(filePath)
	})
	api.POST("/auth/register", authLimitMiddleware, func(c *gin.Context) {
		var in struct {
			Name, Email, Password string
			Age                   int
			HeightCM, WeightKG    float64
			PreferredFoot, Bio    string
			Name          string  `json:"name"`
			Email         string  `json:"email"`
			Password      string  `json:"password"`
			Age           int     `json:"age"`
			HeightCM      float64 `json:"height_cm"`
			WeightKG      float64 `json:"weight_kg"`
			PreferredFoot string  `json:"preferred_foot"`
			Bio           string  `json:"bio"`
		}
		if c.BindJSON(&in) != nil || in.Name == "" || len(in.Password) < 8 {
			c.JSON(400, err("name/email/password(8+) required"))
			return
		}
		hash, e := as.Hash(in.Password)
		if e != nil {
			c.JSON(500, err("password hashing failed"))
			return
		}
		u := models.User{Name: in.Name, Email: strings.ToLower(strings.TrimSpace(in.Email)), PasswordHash: hash, Age: in.Age, HeightCM: in.HeightCM, WeightKG: in.WeightKG, PreferredFoot: in.PreferredFoot, Bio: in.Bio}
		if e = db.Create(&u).Error; e != nil {
			c.JSON(409, err("email already registered"))
			return
		}
		tok, _ := as.Token(u.ID.String())
		c.JSON(201, gin.H{"success": true, "data": gin.H{"token": tok, "user": u}})
	})
	api.POST("/auth/login", authLimitMiddleware, func(c *gin.Context) {
		var in struct{ Email, Password string }
		if c.BindJSON(&in) != nil {
			c.JSON(400, err("invalid request"))
			return
		}
		var u models.User
		if e := db.Where("email=?", strings.ToLower(strings.TrimSpace(in.Email))).First(&u).Error; e != nil || !as.Check(u.PasswordHash, in.Password) {
			c.JSON(401, err("invalid credentials"))
			return
		}
		tok, _ := as.Token(u.ID.String())
		c.JSON(200, gin.H{"success": true, "data": gin.H{"token": tok, "user": u}})
	})
	sec := api.Group("")
	sec.Use(as.Middleware())
	sec.GET("/users/me", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		var u models.User
		if e := db.First(&u, uid).Error; e != nil {
			c.JSON(404, err("user not found"))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": u})
	})
	sec.PUT("/users/me", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		var in struct {
			Name          *string  `json:"name"`
			Age           *int     `json:"age"`
			HeightCM      *float64 `json:"height_cm"`
			WeightKG      *float64 `json:"weight_kg"`
			PreferredFoot *string  `json:"preferred_foot"`
			Bio           *string  `json:"bio"`
		}
		if c.BindJSON(&in) != nil {
			c.JSON(400, err("invalid request"))
			return
		}
		var u models.User
		if e := db.First(&u, uid).Error; e != nil {
			c.JSON(404, err("user not found"))
			return
		}
		updates := map[string]interface{}{}
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			updates["name"] = strings.TrimSpace(*in.Name)
		}
		if in.Age != nil {
			updates["age"] = *in.Age
		}
		if in.HeightCM != nil {
			updates["height_cm"] = *in.HeightCM
		}
		if in.WeightKG != nil {
			updates["weight_kg"] = *in.WeightKG
		}
		if in.PreferredFoot != nil {
			updates["preferred_foot"] = strings.TrimSpace(*in.PreferredFoot)
		}
		if in.Bio != nil {
			updates["bio"] = strings.TrimSpace(*in.Bio)
		}
		if len(updates) > 0 {
			if e := db.Model(&u).Updates(updates).Error; e != nil {
				c.JSON(500, err("failed to update profile"))
				return
			}
			if e := db.First(&u, uid).Error; e != nil {
				c.JSON(500, err("failed to reload profile"))
				return
			}
		}
		c.JSON(200, gin.H{"success": true, "data": u})
	})
	sec.GET("/sports", func(c *gin.Context) {
		var s []models.Sport
		db.Where("active=true").Find(&s)
		log.Println("/sports>>>>", s)
		c.JSON(200, gin.H{"success": true, "data": s})
	})
	sec.POST("/groups", func(c *gin.Context) {
		var in struct {
			Name, Description, City, Privacy string
			SportID                          string `json:"sport_id"`
		}
		if c.BindJSON(&in) != nil || in.Name == "" {
			c.JSON(400, err("group name is required"))
			return
		}
		uid := mustUUID(auth.UserID(c))
		// validate sport id
		if strings.TrimSpace(in.SportID) == "" {
			c.JSON(400, err("sport_id is required"))
			return
		}
		sidParsed, sidErr := uuid.Parse(in.SportID)
		if sidErr != nil {
			c.JSON(400, err("invalid sport_id"))
			return
		}
		sid := sidParsed
		g, e := gs.Create(in.Name, in.Description, in.City, strings.ToUpper(defaultStr(in.Privacy, "PRIVATE")), sid, uid)
		if e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		c.JSON(201, gin.H{"success": true, "data": g})
	})
	sec.GET("/groups", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		var gsx []models.Group
		db.Joins("JOIN group_members gm ON gm.group_id=groups.id").Where("gm.user_id=? AND gm.status='ACTIVE'", uid).Order("groups.created_at DESC").Find(&gsx)
		log.Println("/groups>>>>", gsx)
		c.JSON(200, gin.H{"success": true, "data": gsx})
	})
	sec.GET("/groups/:id", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		if !mustMember(db, gid, mustUUID(auth.UserID(c))) {
			c.JSON(403, err("not a group member"))
			return
		}
		var g models.Group
		if e := db.First(&g, gid).Error; e != nil {
			c.JSON(404, err("group not found"))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": g})
	})
	sec.GET("/groups/:id/members", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		var msx []models.GroupMember
		db.Where("group_id=? AND status='ACTIVE'", gid).Find(&msx)
		var ids []uuid.UUID
		for _, m := range msx {
			ids = append(ids, m.UserID)
		}
		var us []models.User
		if len(ids) > 0 {
			db.Where("id IN ?", ids).Find(&us)
		}
		c.JSON(200, gin.H{"success": true, "data": gin.H{"members": msx, "users": us}})
	})
	updateMemberRoleHandler := func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		targetUID := mustUUID(c.Param("userId"))
		actorUID := mustUUID(auth.UserID(c))

		if !isGroupAdmin(db, gid, actorUID) {
			c.JSON(403, err("admin access required"))
			return
		}

		var in struct {
			Role string `json:"role"`
		}
		if e := c.ShouldBindJSON(&in); e != nil {
			c.JSON(400, err("invalid input"))
			return
		}
		newRole := strings.ToUpper(strings.TrimSpace(in.Role))
		if newRole != "ADMIN" && newRole != "MEMBER" {
			c.JSON(400, err("role must be ADMIN or MEMBER"))
			return
		}

		var targetMember models.GroupMember
		if e := db.Where("group_id=? AND user_id=? AND status='ACTIVE'", gid, targetUID).First(&targetMember).Error; e != nil {
			c.JSON(404, err("member not found in group"))
			return
		}

		if targetMember.Role == "OWNER" {
			c.JSON(400, err("cannot change owner role"))
			return
		}

		targetMember.Role = newRole
		if e := db.Save(&targetMember).Error; e != nil {
			c.JSON(500, err("failed to update member role"))
			return
		}

		var g models.Group
		db.First(&g, gid)
		groupName := g.Name
		if groupName == "" {
			groupName = "the squad"
		}

		var notifTitle, notifMsg string
		if newRole == "ADMIN" {
			notifTitle = "Promoted to Admin"
			notifMsg = "You are now an admin of " + groupName + "."
		} else {
			notifTitle = "Role Updated"
			notifMsg = "Your role in " + groupName + " is now Member."
		}

		n := models.Notification{
			UserID:     targetUID,
			GroupID:    &gid,
			Type:       "ROLE_UPDATED",
			Title:      notifTitle,
			Message:    notifMsg,
			EntityType: "GROUP",
			EntityID:   &gid,
		}
		_ = db.Create(&n).Error

		c.JSON(200, gin.H{"success": true, "data": targetMember})
	}
	sec.PUT("/groups/:id/members/:userId/role", updateMemberRoleHandler)
	sec.POST("/groups/:id/members/:userId/role", updateMemberRoleHandler)
	sec.GET("/groups/:id/join-requests", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !isGroupAdmin(db, gid, uid) {
			c.JSON(403, err("admin access required"))
			return
		}
		var requests []models.GroupJoinRequest
		db.Where("group_id=? AND status='PENDING'", gid).Order("created_at ASC").Find(&requests)
		// Older app versions could create duplicates; show only the oldest one.
		uniqueRequests := make([]models.GroupJoinRequest, 0, len(requests))
		seenUsers := make(map[uuid.UUID]bool)
		for _, request := range requests {
			if !seenUsers[request.UserID] {
				seenUsers[request.UserID] = true
				uniqueRequests = append(uniqueRequests, request)
			}
		}
		requests = uniqueRequests
		var userIDs []uuid.UUID
		for _, request := range requests {
			userIDs = append(userIDs, request.UserID)
		}
		var users []models.User
		if len(userIDs) > 0 {
			db.Where("id IN ?", userIDs).Find(&users)
		}
		c.JSON(200, gin.H{"success": true, "data": gin.H{"requests": requests, "users": users}})
	})
	sec.GET("/notifications", func(c *gin.Context) {
		notifications := []models.Notification{}
		db.Where("user_id=?", mustUUID(auth.UserID(c))).Order("created_at DESC").Limit(50).Find(&notifications)
		c.JSON(200, gin.H{"success": true, "data": notifications})
	})
	sec.GET("/notifications/unread-count", func(c *gin.Context) {
		var count int64
		db.Model(&models.Notification{}).Where("user_id=? AND read_at IS NULL", mustUUID(auth.UserID(c))).Count(&count)
		c.JSON(200, gin.H{"success": true, "data": gin.H{"unread_count": count}})
	})
	sec.POST("/notifications/:id/read", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		nid := mustUUID(c.Param("id"))
		if nid == uuid.Nil {
			c.JSON(400, err("invalid notification id"))
			return
		}
		now := time.Now()
		db.Model(&models.Notification{}).Where("id=? AND user_id=?", nid, uid).Update("read_at", now)
		c.JSON(200, gin.H{"success": true})
	})
	sec.POST("/notifications/read-all", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		now := time.Now()
		db.Model(&models.Notification{}).Where("user_id=? AND read_at IS NULL", uid).Update("read_at", now)
		c.JSON(200, gin.H{"success": true})
	})
	sec.DELETE("/notifications/:id", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		nid := mustUUID(c.Param("id"))
		if nid == uuid.Nil {
			c.JSON(400, err("invalid notification id"))
			return
		}
		db.Where("id=? AND user_id=?", nid, uid).Delete(&models.Notification{})
		c.JSON(200, gin.H{"success": true})
	})
	sec.DELETE("/notifications/clear-all", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		db.Where("user_id=?", uid).Delete(&models.Notification{})
		c.JSON(200, gin.H{"success": true})
	})
	sec.POST("/groups/join", func(c *gin.Context) {

		var in struct {
			InviteCode string `json:"invite_code"`
		}

		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(400, gin.H{
				"success": false,
				"error":   "invalid request body",
				"details": err.Error(),
			})
			return
		}

		if strings.TrimSpace(in.InviteCode) == "" {
			c.JSON(400, gin.H{
				"success": false,
				"error":   "invite code required",
			})
			return
		}

		r, e := gs.JoinByCode(
			strings.TrimSpace(in.InviteCode),
			mustUUID(auth.UserID(c)),
		)

		if e != nil {
			c.JSON(400, err(e.Error()))
			return
		}

		c.JSON(201, gin.H{
			"success": true,
			"data":    r,
		})
	})
	sec.POST("/groups/:id/join-requests/:requestId/approve", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		var request models.GroupJoinRequest
		if e := db.First(&request, mustUUID(c.Param("requestId"))).Error; e != nil || request.GroupID != gid {
			c.JSON(404, err("join request not found"))
			return
		}
		r, e := gs.Approve(mustUUID(c.Param("requestId")), mustUUID(auth.UserID(c)))
		if e != nil {
			c.JSON(403, err(e.Error()))
			return
		}
		var g models.Group
		db.First(&g, gid)
		var newUser models.User
		db.First(&newUser, request.UserID)
		userName := newUser.Name
		if userName == "" {
			userName = "A new player"
		}
		notifyGroupMembers(db, gid, &request.UserID, "MEMBER_JOINED", "New Squad Member", fmt.Sprintf("%s has joined %s! Welcome them to the squad.", userName, g.Name), "GROUP", &gid)
		c.JSON(200, gin.H{"success": true, "data": r})
	})
	sec.GET("/groups/:id/ratings/:userId", func(c *gin.Context) {
		avg, n, e := rs.Average(mustUUID(c.Param("id")), mustUUID(c.Param("userId")))
		if e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": gin.H{"rating": avg, "count": n}})
	})
	sec.POST("/groups/:id/ratings", func(c *gin.Context) {
		var in struct {
			RatedUserID string
			Overall     float64
			Attributes  map[string]float64
		}
		if c.BindJSON(&in) != nil {
			c.JSON(400, err("invalid request"))
			return
		}
		p, e := rs.Upsert(mustUUID(c.Param("id")), mustUUID(auth.UserID(c)), mustUUID(in.RatedUserID), in.Overall, in.Attributes)
		if e != nil {
			c.JSON(400, err(e.Error()))
			return
		}
		c.JSON(201, gin.H{"success": true, "data": p})
	})
	sec.GET("/groups/:id/chat", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		var msgs []models.ChatMessage
		if e := db.Where("group_id=?", gid).Order("created_at ASC").Limit(100).Find(&msgs).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": msgs})
	})
	sec.POST("/groups/:id/chat", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		var in struct{ Content string }
		if c.BindJSON(&in) != nil || strings.TrimSpace(in.Content) == "" {
			c.JSON(400, err("message cannot be empty"))
			return
		}
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		m := models.ChatMessage{GroupID: gid, SenderID: uid, MessageType: "TEXT", Content: in.Content}
		if e := db.Create(&m).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		hub.Broadcast(gid.String(), ws.Event{Type: "CHAT_MESSAGE", Data: m})
		c.JSON(201, gin.H{"success": true, "data": m})
	})
	sec.GET("/ws/groups/:id", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.AbortWithStatus(403)
			return
		}
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, e := up.Upgrade(c.Writer, c.Request, nil)
		if e != nil {
			return
		}
		cl := &ws.Client{Conn: conn, Key: gid.String(), UserID: uid.String()}
		hub.Add(cl)
		defer hub.Remove(cl)
		for {
			if _, _, e := conn.ReadMessage(); e != nil {
				return
			}
		}
	})
	sec.GET("/ws/matches/:id", func(c *gin.Context) {
		var m models.Match
		if db.First(&m, mustUUID(c.Param("id"))).Error != nil {
			c.AbortWithStatus(404)
			return
		}
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, m.GroupID, uid) {
			c.AbortWithStatus(403)
			return
		}
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, e := up.Upgrade(c.Writer, c.Request, nil)
		if e != nil {
			return
		}
		cl := &ws.Client{Conn: conn, Key: m.ID.String(), UserID: uid.String()}
		hub.Add(cl)
		defer hub.Remove(cl)
		for {
			if _, _, e := conn.ReadMessage(); e != nil {
				return
			}
		}
	})
	sec.POST("/groups/:id/venues", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !mustAdmin(db, gid, uid) {
			c.JSON(403, err("admin only"))
			return
		}
		var in struct {
			Name, Address, GoogleMapsURL, Notes string
			Latitude, Longitude                 *float64
			IsDefault                           bool
		}
		if c.BindJSON(&in) != nil || in.Name == "" {
			c.JSON(400, err("venue name required"))
			return
		}
		v := models.Venue{GroupID: gid, Name: in.Name, Address: in.Address, GoogleMapsURL: in.GoogleMapsURL, Notes: in.Notes, Latitude: in.Latitude, Longitude: in.Longitude, IsDefault: in.IsDefault}
		if e := db.Create(&v).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		c.JSON(201, gin.H{"success": true, "data": v})
	})
	sec.GET("/groups/:id/venues", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		var vs []models.Venue
		db.Where("group_id=?", gid).Find(&vs)
		c.JSON(200, gin.H{"success": true, "data": vs})
	})
	sec.POST("/groups/:id/matches", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		if !mustAdmin(db, gid, mustUUID(auth.UserID(c))) {
			c.JSON(403, err("admin only"))
			return
		}
		var in struct {
			Name            string `json:"name"`
			ScheduledAt     string `json:"scheduled_at"`
			VenueID         string `json:"venue_id"`
			Format          string `json:"format"`
			Notes           string `json:"notes"`
			DurationMinutes int    `json:"duration_minutes"`
			TeamCount       int    `json:"team_count"`
			PlayersPerTeam  int    `json:"players_per_team"`
			MaxPlayers      int    `json:"max_players"`
		}
		if c.BindJSON(&in) != nil || in.Name == "" {
			c.JSON(400, err("match name required"))
			return
		}
		var g models.Group
		db.First(&g, gid)
		t, e := time.Parse(time.RFC3339, in.ScheduledAt)
		if e != nil {
			t = time.Now().Add(24 * time.Hour)
		}
		m := models.Match{GroupID: gid, SportID: g.SportID, Name: in.Name, ScheduledAt: t, Format: in.Format, DurationMinutes: in.DurationMinutes, TeamCount: defaultInt(in.TeamCount, 2), PlayersPerTeam: in.PlayersPerTeam, MaxPlayers: in.MaxPlayers, Notes: in.Notes, Status: "UPCOMING"}
		if in.VenueID != "" {
			id := mustUUID(in.VenueID)
			m.VenueID = &id
		}
		if e = db.Create(&m).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		schedulerID := mustUUID(auth.UserID(c))
		notifyGroupMembers(db, gid, &schedulerID, "MATCH_SCHEDULED", "New Match Scheduled", fmt.Sprintf("'%s' has been scheduled for %s in %s. Check the lineup and RSVP!", m.Name, m.ScheduledAt.Format("Mon, Jan 02 • 15:04"), g.Name), "MATCH", &m.ID)
		c.JSON(201, gin.H{"success": true, "data": m})
	})
	sec.GET("/groups/:id/matches", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		var msx []models.Match
		db.Where("group_id=?", gid).Order("scheduled_at DESC").Find(&msx)
		c.JSON(200, gin.H{"success": true, "data": msx})
	})
	// All match REST reads and writes require an existing match and active membership.
	sec.Use(func(c *gin.Context) {
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
		if e := db.First(&m, id).Error; e != nil {
			if e == gorm.ErrRecordNotFound {
				c.AbortWithStatusJSON(404, err("match not found"))
			} else {
				c.AbortWithStatusJSON(500, err("could not load match"))
			}
			return
		}
		if !mustMember(db, m.GroupID, mustUUID(auth.UserID(c))) {
			c.AbortWithStatusJSON(403, err("not a group member"))
			return
		}
		c.Set("match", m)
		c.Next()
	})
	sec.GET("/matches/:id", func(c *gin.Context) {
		c.JSON(200, gin.H{"success": true, "data": c.MustGet("match")})
	})
	sec.POST("/matches/:id/attendance", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		var in struct {
			Status string `json:"status"`
		}
		if c.ShouldBindJSON(&in) != nil {
			c.JSON(400, err("invalid request"))
			return
		}
		st := strings.ToUpper(strings.TrimSpace(in.Status))
		if st != "GOING" && st != "MAYBE" && st != "NOT_GOING" {
			c.JSON(400, err("invalid attendance status"))
			return
		}
		if !mustMatchMember(db, mid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		// Cleanly remove any existing attendance for this match & user to prevent duplicates
		db.Where("match_id = ? AND user_id = ?", mid, uid).Delete(&models.Attendance{})
		a := models.Attendance{
			MatchID:     mid,
			UserID:      uid,
			Status:      st,
			RespondedAt: time.Now(),
		}
		if e := db.Create(&a).Error; e != nil {
			c.JSON(500, err("could not record attendance"))
			return
		}
		var u models.User
		db.First(&u, uid)
		c.JSON(200, gin.H{"success": true, "data": a, "user": u})
	})
	sec.GET("/matches/:id/attendance", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		var ax []models.Attendance
		if e := db.Where("match_id=?", mid).Order("responded_at DESC, created_at DESC").Find(&ax).Error; e != nil {
			c.JSON(500, err("could not load attendance"))
			return
		}
		// Deduplicate by user_id keeping the newest response
		seenUsers := make(map[uuid.UUID]bool)
		deduped := make([]models.Attendance, 0, len(ax))
		for _, a := range ax {
			if !seenUsers[a.UserID] {
				seenUsers[a.UserID] = true
				deduped = append(deduped, a)
			}
		}
		ax = deduped
		var uids []uuid.UUID
		for _, a := range ax {
			uids = append(uids, a.UserID)
		}
		var users []models.User
		if len(uids) > 0 {
			db.Where("id IN ?", uids).Find(&users)
		}
		userMap := make(map[uuid.UUID]models.User)
		for _, u := range users {
			userMap[u.ID] = u
		}
		type itemOut struct {
			models.Attendance
			User *models.User `json:"user,omitempty"`
		}
		out := make([]itemOut, 0, len(ax))
		for _, a := range ax {
			var uPtr *models.User
			if u, ok := userMap[a.UserID]; ok {
				uCopy := u
				uPtr = &uCopy
			}
			out = append(out, itemOut{Attendance: a, User: uPtr})
		}
		c.JSON(200, gin.H{"success": true, "data": out, "users": users})
	})
	sec.POST("/matches/:id/generate-teams", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		var m models.Match
		db.First(&m, mid)
		if !mustAdmin(db, m.GroupID, mustUUID(auth.UserID(c))) {
			c.JSON(403, err("admin only"))
			return
		}
		var ax []models.Attendance
		db.Where("match_id=? AND status='GOING'", mid).Find(&ax)
		ids := []uuid.UUID{}
		for _, a := range ax {
			ids = append(ids, a.UserID)
		}
		var us []models.User
		if len(ids) > 0 {
			db.Where("id IN ?", ids).Find(&us)
		}
		ratings := map[uuid.UUID]float64{}
		for _, u := range us {
			ratings[u.ID], _, _ = rs.Average(m.GroupID, u.ID)
		}
		ps := []team.Player{}
		for _, u := range us {
			r := ratings[u.ID]
			if r == 0 {
				r = 5
			}
			ps = append(ps, team.Player{UserID: u.ID, Name: u.Name, Rating: r})
		}
		teams := team.Generate(ps, defaultInt(m.TeamCount, 2))
		tx := db.Begin()

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

			// 5. Add players to the newly created team
			for _, x := range p {

				member := models.TeamMember{
					TeamID: tm.ID,
					UserID: x.UserID,
				}

				if e := tx.Create(&member).Error; e != nil {
					tx.Rollback()
					c.JSON(500, err(e.Error()))
					return
				}
			}

			out = append(out, tm)
		}

		// 6. Commit
		if e := tx.Commit().Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}

		c.JSON(200, gin.H{
			"success": true,
			"data":    out,
		})
	})
	sec.GET("/matches/:id/teams", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		var ts []models.Team
		if e := db.Where("match_id=?", mid).Order("created_at ASC, id ASC").Find(&ts).Error; e != nil {
			c.JSON(500, err("could not load teams"))
			return
		}
		type teamOut struct {
			models.Team
			Members []models.TeamMember `json:"members"`
		}
		out := make([]teamOut, 0, len(ts))
		for _, t := range ts {
			var mem []models.TeamMember
			db.Where("team_id=?", t.ID).Find(&mem)
			out = append(out, teamOut{t, mem})
		}
		c.JSON(200, gin.H{"success": true, "data": out})
	})
	sec.POST("/matches/:id/ai-poster", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !mustMatchMember(db, mid, uid) {
			c.JSON(403, err("only squad members can generate a match poster"))
			return
		}

		var in struct {
			APIKey string `json:"apiKey"`
			Style  string `json:"style"`
		}
		_ = c.ShouldBindJSON(&in)

		if imageClient.BaseURL == "" {
			imageClient.BaseURL = os.Getenv("IMAGE_GENERATION_URL")
		}
		if imageClient.APIKey == "" {
			imageClient.APIKey = os.Getenv("IMAGE_GENERATION_API_KEY")
		}
		if imageClient.BaseURL == "" {
			c.JSON(400, err("IMAGE_GENERATION_URL not configured on backend server. Please set IMAGE_GENERATION_URL in backend/.env"))
			return
		}

		var m models.Match
		if db.First(&m, mid).Error != nil {
			c.JSON(404, err("match not found"))
			return
		}

		var ts []models.Team
		db.Where("match_id=?", mid).Order("created_at ASC, id ASC").Find(&ts)
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
			db.Where("team_id = ?", t.ID).Find(&tMembers)
			var uids []uuid.UUID
			for _, tm := range tMembers {
				uids = append(uids, tm.UserID)
			}
			var users []models.User
			if len(uids) > 0 {
				db.Where("id IN ?", uids).Find(&users)
			}
			var playerNames []string
			for _, u := range users {
				playerNames = append(playerNames, u.Name)
			}
			lineups = append(lineups, teamLineup{Name: t.Name, Players: playerNames})
		}

		var g models.Group
		db.First(&g, m.GroupID)

		var ven models.Venue
		venueName := "Matchday Turf Arena"
		if m.VenueID != nil && db.First(&ven, *m.VenueID).Error == nil && ven.Name != "" {
			venueName = ven.Name
		}

		var sp models.Sport
		sportLower := "football"
		if db.First(&sp, m.SportID).Error == nil && sp.Name != "" {
			sportLower = strings.ToLower(sp.Name)
		}

		var matchTeamBlocks []string
		for i, line := range lineups {
			teamName := line.Name
			if strings.TrimSpace(teamName) == "" {
				teamName = fmt.Sprintf("Team %d", i+1)
			}
			playersStr := strings.Join(line.Players, ", ")
			if strings.TrimSpace(playersStr) == "" {
				playersStr = "Squad Players"
			}
			matchTeamBlocks = append(matchTeamBlocks, fmt.Sprintf("TEAM %d: %s\nPLAYERS: %s", i+1, teamName, playersStr))
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

		prompt := fmt.Sprintf(
			"Create a premium modern %s match poster for a casual recreational game.\n\n"+
				"**STYLE:**\n\n"+
				"* Professional %s promotional poster\n"+
				"* Night stadium, dramatic floodlights, subtle fog\n"+
				"* Dark cinematic background with %s pitch\n"+
				"* Bold modern typography\n"+
				"* Clean, minimal, premium sports design\n"+
				"* Vertical 4:5 social-media format\n\n"+
				"**MATCH:**\n"+
				"%s\n\n"+
				"DATE: %s\n"+
				"TIME: %s\n"+
				"VENUE: %s\n\n"+
				"**LAYOUT:**\n\n"+
				"* \"MATCH DAY\" at the top\n"+
				"* Teams prominently displayed with players underneath\n"+
				"* Large centered \"VS\"\n"+
				"* Date, time and venue at the bottom\n"+
				"* Adapt layout for 2 or 3 teams\n"+
				"* Strong hierarchy and mobile readability\n\n"+
				"**IMPORTANT:**\n"+
				"Use exact provided text only. Do not invent players, scores, logos, sponsors, or extra information. No professional club branding.",
			sportLower, sportLower, sportLower, matchSection, dateStr, timeStr, venueName,
		)

		imgBytes, genErr := imageClient.Generate(c.Request.Context(), prompt)
		if genErr != nil {
			c.JSON(500, err(genErr.Error()))
			return
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

		// Persist poster image to disk
		uploadsDir := filepath.Join("uploads", "posters")
		_ = os.MkdirAll(uploadsDir, 0755)
		posterFilename := fmt.Sprintf("%s.png", mid.String())
		posterPath := filepath.Join(uploadsDir, posterFilename)
		if writeErr := os.WriteFile(posterPath, imgBytes, 0644); writeErr != nil {
			log.Printf("Failed to write poster file: %v", writeErr)
		}

		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		host := c.Request.Host
		posterURL := fmt.Sprintf("%s://%s/api/matches/%s/poster-image", scheme, host, mid.String())
		dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(imgBytes))

		// Persist on match
		db.Model(&models.Match{}).Where("id = ?", mid).Update("poster_url", posterURL)

		c.JSON(200, gin.H{
			"success": true,
			"data": gin.H{
				"image_url":  dataURL,
				"poster_url": posterURL,
				"prompt":     prompt,
			},
		})
	})
	sec.POST("/matches/:id/start", func(c *gin.Context) {
		var m models.Match
		db.First(&m, mustUUID(c.Param("id")))
		if !mustAdmin(db, m.GroupID, mustUUID(auth.UserID(c))) {
			c.JSON(403, err("admin only"))
			return
		}
		o, e := ms.Start(m.ID)
		if e != nil {
			c.JSON(400, err(e.Error()))
			return
		}
		var g models.Group
		db.First(&g, m.GroupID)
		notifyGroupMembers(db, m.GroupID, nil, "MATCH_LIVE", "Match is LIVE! ⚽", fmt.Sprintf("'%s' has kicked off in %s! Follow the live scoreline.", m.Name, g.Name), "MATCH", &m.ID)
		c.JSON(200, gin.H{"success": true, "data": o})
	})
	sec.POST("/matches/:id/finish", func(c *gin.Context) {
		var m models.Match
		db.First(&m, mustUUID(c.Param("id")))
		if !mustAdmin(db, m.GroupID, mustUUID(auth.UserID(c))) {
			c.JSON(403, err("admin only"))
			return
		}
		o, e := ms.Finish(m.ID)
		if e != nil {
			c.JSON(400, err(e.Error()))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": o})
	})
	sec.POST("/matches/:id/events", func(c *gin.Context) {
		var m models.Match
		db.First(&m, mustUUID(c.Param("id")))
		if !mustAdmin(db, m.GroupID, mustUUID(auth.UserID(c))) {
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
		ev := models.MatchEvent{EventType: in.EventType, MatchTimeSeconds: in.MatchTimeSeconds, Metadata: in.Metadata, TeamID: in.TeamID, PlayerID: in.PlayerID, AssistPlayerID: in.AssistPlayerID}
		if e := ms.AddEvent(m.ID, &ev); e != nil {
			c.JSON(400, err(e.Error()))
			return
		}
		c.JSON(201, gin.H{"success": true, "data": ev})
	})
	sec.DELETE("/matches/:id/events/:eventId", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		eid := mustUUID(c.Param("eventId"))
		var m models.Match
		if e := db.First(&m, mid).Error; e != nil {
			c.JSON(404, err("match not found"))
			return
		}
		if !mustAdmin(db, m.GroupID, mustUUID(auth.UserID(c))) {
			c.JSON(403, err("admin only"))
			return
		}
		if e := ms.DeleteEvent(mid, eid); e != nil {
			c.JSON(400, err(e.Error()))
			return
		}
		c.JSON(200, gin.H{"success": true})
	})
	sec.GET("/matches/:id/events", func(c *gin.Context) {
		var es []models.MatchEvent
		if e := db.Where("match_id=?", mustUUID(c.Param("id"))).Order("match_time_seconds ASC,created_at ASC, id ASC").Find(&es).Error; e != nil {
			c.JSON(500, err("could not load events"))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": es})
	})
	sec.GET("/matches/:id/result", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		var result models.MatchResult
		if e := db.Where("match_id=?", mid).First(&result).Error; e != nil {
			c.JSON(404, err("match result not found"))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": result})
	})
	sec.POST("/matches/:id/finalize", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		var m models.Match
		db.First(&m, mid)
		if !mustAdmin(db, m.GroupID, mustUUID(auth.UserID(c))) {
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
		if e := ms.Finalize(mid, &r0); e != nil {
			c.JSON(400, err(e.Error()))
			return
		}
		var g models.Group
		db.First(&g, m.GroupID)
		notifyGroupMembers(db, m.GroupID, nil, "MATCH_FINALIZED", "Match Result Finalized 🏆", fmt.Sprintf("Final score recorded for '%s': %d - %d in %s. Check MVP & squad stats!", m.Name, in.HomeScore, in.AwayScore, g.Name), "MATCH", &mid)
		c.JSON(200, gin.H{"success": true, "data": r0})
	})
	sec.GET("/groups/:id/leaderboard", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		var st []models.PlayerStatistics
		db.Where("group_id=?", gid).Order("goals DESC,assists DESC,matches DESC").Limit(50).Find(&st)
		c.JSON(200, gin.H{"success": true, "data": st})
	})
	log.Printf("SquadUp listening on :%s", cfg.Port)
	// Bind explicitly to IPv4 so physical devices can reach the server through
	// the Mac's LAN address (for example, 192.168.x.x).
	log.Fatal(http.ListenAndServe("0.0.0.0:"+cfg.Port, r))
}

func seed(db *gorm.DB) {
	var s models.Sport
	if db.Where("name=?", "Football").First(&s).Error == nil {
		return
	}
	s = models.Sport{Name: "Football", Icon: "⚽", Active: true}
	db.Create(&s)
	pos := []string{"Goalkeeper", "Defender", "Full Back", "Centre Back", "Midfielder", "Winger", "Striker"}
	codes := []string{"GK", "DEF", "FB", "CB", "MF", "WG", "ST"}
	for i, n := range pos {
		db.Create(&models.SportPosition{SportID: s.ID, Name: n, Code: codes[i], SortOrder: i})
	}
}
func cors(origin string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		if c.Request.Method == "OPTIONS" {
			c.Status(204)
			return
		}
		c.Next()
	}
}
func err(msg string) gin.H { return gin.H{"success": false, "error": gin.H{"message": msg}} }
func mustUUID(s string) uuid.UUID {
	x, e := uuid.Parse(s)
	if e != nil {
		// return Nil instead of panicking; callers should validate where appropriate
		return uuid.Nil
	}
	return x
}
func defaultStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func defaultInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}
func mustMember(db *gorm.DB, gid, uid uuid.UUID) bool {
	var c int64
	db.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND status='ACTIVE'", gid, uid).Count(&c)
	return c > 0
}

func isGroupAdmin(db *gorm.DB, gid, uid uuid.UUID) bool {
	var g models.Group
	if db.Select("owner_id").First(&g, gid).Error == nil && g.OwnerID == uid {
		return true
	}
	var m models.GroupMember
	return db.Where("group_id=? AND user_id=? AND status='ACTIVE' AND role IN ?", gid, uid, []string{"OWNER", "ADMIN"}).First(&m).Error == nil
}
func mustAdmin(db *gorm.DB, gid, uid uuid.UUID) bool {
	var g models.Group
	if db.Select("owner_id").First(&g, gid).Error == nil && g.OwnerID == uid {
		return true
	}
	var m models.GroupMember
	if db.Where("group_id=? AND user_id=? AND status='ACTIVE'", gid, uid).First(&m).Error != nil {
		return false
	}
	return m.Role == "OWNER" || m.Role == "ADMIN"
}
func mustMatchMember(db *gorm.DB, mid, uid uuid.UUID) bool {
	var m models.Match
	if db.First(&m, mid).Error != nil {
		return false
	}
	return mustMember(db, m.GroupID, uid)
}

func notifyGroupMembers(db *gorm.DB, groupID uuid.UUID, excludeUserID *uuid.UUID, notifType, title, message, entityType string, entityID *uuid.UUID) {
	var members []models.GroupMember
	q := db.Where("group_id = ? AND status = 'ACTIVE'", groupID)
	if excludeUserID != nil && *excludeUserID != uuid.Nil {
		q = q.Where("user_id != ?", *excludeUserID)
	}
	if err := q.Find(&members).Error; err != nil {
		return
	}
	for _, mem := range members {
		n := models.Notification{
			UserID:     mem.UserID,
			GroupID:    &groupID,
			Type:       notifType,
			Title:      title,
			Message:    message,
			EntityType: entityType,
			EntityID:   entityID,
		}
		db.Create(&n)
	}
}

func teamName(i int) string {
	return []string{"Red", "Blue", "Green", "Yellow", "Orange", "Purple"}[i%6]
}
func teamCode(i int) string { return []string{"RED", "BLU", "GRN", "YEL", "ORG", "PUR"}[i%6] }

var _ = http.MethodGet
