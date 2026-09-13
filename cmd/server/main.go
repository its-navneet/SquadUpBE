package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
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
	"squadup/backend/internal/push"
	"squadup/backend/internal/rating"
	"squadup/backend/internal/storage"
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
	go func() {
		var groupIDs []uuid.UUID
		db.Model(&models.Match{}).Where("finalized_at IS NOT NULL").Distinct("group_id").Pluck("group_id", &groupIDs)
		for _, gid := range groupIDs {
			_ = match.RecalculateGroupStats(db, gid)
		}
	}()
	as := auth.New(cfg.JWTSecret)
	hub := ws.New()
	presence := ws.NewPresenceTracker()
	gs := group.New(db)
	rs := rating.New(db)
	ms := match.New(db, hub)
	pushService := push.New(db, cfg.FirebaseProjectID, cfg.FirebaseCredentialsPath, cfg.FirebaseCredentialsJSON)
	models.PushDispatcher = func(userID uuid.UUID, groupID *uuid.UUID, nType, title, message, entityType string, entityID *uuid.UUID) {
		log.Printf("[PushDispatcher] Triggered for user %s: type=%s, title=%q", userID, nType, title)
		data := map[string]string{
			"type":        nType,
			"entity_type": entityType,
		}
		if entityID != nil {
			data["entity_id"] = entityID.String()
		}
		if groupID != nil {
			data["group_id"] = groupID.String()
		}
		pushService.SendPush([]uuid.UUID{userID}, title, message, data)
	}
	imageClient := client.NewImageClient(
		cfg.ImageGenURL,
		cfg.ImageGenKey,
	)
	imgStorage := storage.New(cfg)
	models.MediaSigner = func(bucket, key, rawURL string) string {
		if bucket != "" && key != "" {
			signed, err := imgStorage.Presign(context.Background(), bucket, key)
			if err == nil && signed != "" {
				return signed
			}
		}
		if rawURL != "" {
			return imgStorage.PresignURL(context.Background(), rawURL)
		}
		return ""
	}

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
	_ = r.SetTrustedProxies(nil)
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
		if _, err := uuid.Parse(mid); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid match id"})
			return
		}
		filePath := filepath.Join("uploads", "posters", fmt.Sprintf("%s.png", mid))
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "poster image not found"})
			return
		}
		c.Header("Cache-Control", "public, max-age=86400")
		c.File(filePath)
	})
	api.POST("/upload/image", authLimitMiddleware, func(c *gin.Context) {
		file, header, err := c.Request.FormFile("image")
		if err != nil {
			c.JSON(400, gin.H{"error": "image file required in 'image' field"})
			return
		}
		defer file.Close()

		const maxImageBytes = 2 * 1024 * 1024 // 2MB
		if header.Size > maxImageBytes {
			c.JSON(400, gin.H{"error": "image size must be less than 2MB"})
			return
		}

		data, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
		if err != nil {
			c.JSON(500, gin.H{"error": "failed to read uploaded file"})
			return
		}
		if int64(len(data)) > maxImageBytes {
			c.JSON(400, gin.H{"error": "image size must be less than 2MB"})
			return
		}

		contentType := http.DetectContentType(data)
		validTypes := map[string]string{
			"image/jpeg": ".jpg",
			"image/png":  ".png",
			"image/webp": ".webp",
			"image/gif":  ".gif",
		}
		ext, ok := validTypes[contentType]
		if !ok {
			c.JSON(400, gin.H{"error": "only genuine JPEG, PNG, WebP or GIF images are supported"})
			return
		}

		if contentType == "image/webp" {
			if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
				c.JSON(400, gin.H{"error": "corrupted or invalid WebP image data"})
				return
			}
		} else {
			if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
				c.JSON(400, gin.H{"error": "corrupted or invalid image data"})
				return
			}
		}

		key := fmt.Sprintf("profiles/%s%s", uuid.New().String(), ext)
		imageURL, err := imgStorage.Upload(c.Request.Context(), key, data, contentType)
		if err != nil {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to upload image: %v", err)})
			return
		}

		if strings.HasPrefix(imageURL, "/") {
			scheme := "http"
			if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			imageURL = fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, imageURL)
		}

		c.JSON(200, gin.H{
			"success": true,
			"data": gin.H{
				"url":    imageURL,
				"key":    key,
				"bucket": imgStorage.BucketName(),
			},
		})
	})
	api.POST("/auth/register", authLimitMiddleware, func(c *gin.Context) {
		var in struct {
			Name               string  `json:"name"`
			Email              string  `json:"email"`
			Password           string  `json:"password"`
			DateOfBirth        string  `json:"date_of_birth"`
			Age                int     `json:"age"`
			HeightCM           float64 `json:"height_cm"`
			WeightKG           float64 `json:"weight_kg"`
			PreferredFoot      string  `json:"preferred_foot"`
			Bio                string  `json:"bio"`
			ProfilePhotoURL    string  `json:"profile_photo_url"`
			ProfilePhotoBucket string  `json:"profile_photo_bucket"`
			ProfilePhotoKey    string  `json:"profile_photo_key"`
			Position           string  `json:"position"`
			KitNumber          int     `json:"kit_number"`
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

		cleanDOB := strings.TrimSpace(in.DateOfBirth)
		computedAge := in.Age
		if cleanDOB != "" {
			calcAge, errVal := models.CalculateAgeFromDOB(cleanDOB)
			if errVal != nil {
				c.JSON(400, err("invalid date of birth format, expected YYYY-MM-DD"))
				return
			}
			if calcAge < 5 || calcAge > 120 {
				c.JSON(400, err("age calculated from date of birth must be between 5 and 120"))
				return
			}
			computedAge = calcAge
		} else if computedAge < 5 || computedAge > 120 {
			c.JSON(400, err("date of birth is required"))
			return
		}

		if in.KitNumber < 0 || in.KitNumber > 99 {
			c.JSON(400, err("jersey number must be between 1 and 99"))
			return
		}

		pos := normalizePosition(in.Position)
		if pos == "" {
			pos = "MID"
		}

		photoURL := cleanStoredURL(in.ProfilePhotoURL)
		photoBucket := strings.TrimSpace(in.ProfilePhotoBucket)
		photoKey := strings.TrimSpace(in.ProfilePhotoKey)
		if photoKey == "" && photoURL != "" {
			photoBucket, photoKey = storage.ExtractBucketAndKey(photoURL, imgStorage.BucketName())
		}
		if photoBucket == "" && photoKey != "" {
			photoBucket = imgStorage.BucketName()
		}

		u := models.User{
			Name:               in.Name,
			Email:              strings.ToLower(strings.TrimSpace(in.Email)),
			PasswordHash:       hash,
			DateOfBirth:        cleanDOB,
			Age:                computedAge,
			HeightCM:           in.HeightCM,
			WeightKG:           in.WeightKG,
			PreferredFoot:      in.PreferredFoot,
			Bio:                in.Bio,
			ProfilePhotoURL:    photoURL,
			ProfilePhotoBucket: photoBucket,
			ProfilePhotoKey:    photoKey,
			Position:           pos,
			KitNumber:          in.KitNumber,
		}
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
	computeCareerStats := func(u *models.User) *models.CareerStats {
		var stats []models.PlayerStatistics
		db.Where("user_id = ?", u.ID).Find(&stats)

		cs := &models.CareerStats{}

		for _, s := range stats {
			cs.Matches += s.Matches
			cs.Wins += s.Wins
			cs.Draws += s.Draws
			cs.Losses += s.Losses
			cs.Goals += s.Goals
			cs.Assists += s.Assists
			cs.MVP += s.MVP
			cs.CleanSheets += s.CleanSheets
		}

		if cs.Matches > 0 {
			cs.WinRate = math.Round((float64(cs.Wins)/float64(cs.Matches))*1000) / 10
		}
		return cs
	}
	sec.GET("/users/me", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		var u models.User
		if e := db.First(&u, uid).Error; e != nil {
			c.JSON(404, err("user not found"))
			return
		}
		u.CareerStats = computeCareerStats(&u)
		c.JSON(200, gin.H{"success": true, "data": u})
	})
	sec.GET("/users/:id", func(c *gin.Context) {
		uid := mustUUID(c.Param("id"))
		if uid == uuid.Nil {
			c.JSON(400, err("invalid user id"))
			return
		}
		var u models.User
		if e := db.First(&u, uid).Error; e != nil {
			c.JSON(404, err("user not found"))
			return
		}
		u.CareerStats = computeCareerStats(&u)
		c.JSON(200, gin.H{"success": true, "data": u})
	})
	sec.PUT("/users/me", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		var in struct {
			Name               *string  `json:"name"`
			DateOfBirth        *string  `json:"date_of_birth"`
			Age                *int     `json:"age"`
			HeightCM           *float64 `json:"height_cm"`
			WeightKG           *float64 `json:"weight_kg"`
			PreferredFoot      *string  `json:"preferred_foot"`
			Bio                *string  `json:"bio"`
			ProfilePhotoURL    *string  `json:"profile_photo_url"`
			ProfilePhotoBucket *string  `json:"profile_photo_bucket"`
			ProfilePhotoKey    *string  `json:"profile_photo_key"`
			Position           *string  `json:"position"`
			KitNumber          *int     `json:"kit_number"`
			FavouriteClub      *string  `json:"favourite_club"`
			FavouritePlayer    *string  `json:"favourite_player"`
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
		if in.DateOfBirth != nil && strings.TrimSpace(*in.DateOfBirth) != "" {
			cleanDOB := strings.TrimSpace(*in.DateOfBirth)
			if u.DateOfBirth != "" && cleanDOB != u.DateOfBirth {
				c.JSON(400, err("date of birth cannot be modified"))
				return
			}
			if u.DateOfBirth == "" {
				calcAge, errVal := models.CalculateAgeFromDOB(cleanDOB)
				if errVal != nil {
					c.JSON(400, err("invalid date of birth format, expected YYYY-MM-DD"))
					return
				}
				if calcAge < 5 || calcAge > 120 {
					c.JSON(400, err("age calculated from date of birth must be between 5 and 120"))
					return
				}
				updates["date_of_birth"] = cleanDOB
				updates["age"] = calcAge
			}
		}
		if in.Age != nil && u.DateOfBirth == "" && updates["date_of_birth"] == nil {
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
		var oldKeyToDelete, oldBucketToDelete string
		if in.ProfilePhotoURL != nil || in.ProfilePhotoKey != nil {
			var newBucket, newKey, newCleanURL string
			if in.ProfilePhotoKey != nil && *in.ProfilePhotoKey != "" {
				newKey = strings.TrimSpace(*in.ProfilePhotoKey)
				newBucket = imgStorage.BucketName()
				if in.ProfilePhotoBucket != nil && *in.ProfilePhotoBucket != "" {
					newBucket = strings.TrimSpace(*in.ProfilePhotoBucket)
				}
				newCleanURL = newKey
			} else if in.ProfilePhotoURL != nil {
				raw := strings.TrimSpace(*in.ProfilePhotoURL)
				if raw != "" {
					newBucket, newKey = storage.ExtractBucketAndKey(raw, imgStorage.BucketName())
					newCleanURL = cleanStoredURL(raw)
				}
			}

			// Determine old bucket and key
			oldKey := u.ProfilePhotoKey
			oldBucket := u.ProfilePhotoBucket
			if oldKey == "" && u.ProfilePhotoURL != "" {
				oldBucket, oldKey = storage.ExtractBucketAndKey(u.ProfilePhotoURL, imgStorage.BucketName())
			}
			if oldBucket == "" {
				oldBucket = imgStorage.BucketName()
			}

			if oldKey != "" && oldKey != newKey {
				oldKeyToDelete = oldKey
				oldBucketToDelete = oldBucket
			}

			updates["profile_photo_bucket"] = newBucket
			updates["profile_photo_key"] = newKey
			updates["profile_photo_url"] = newCleanURL
		}
		if in.Position != nil {
			updates["position"] = normalizePosition(*in.Position)
		}
		if in.KitNumber != nil {
			newKit := *in.KitNumber
			if newKit < 0 || newKit > 99 {
				c.JSON(400, err("jersey number must be between 1 and 99"))
				return
			}
			if newKit > 0 && newKit != u.KitNumber {
				var activeGroupIDs []uuid.UUID
				db.Model(&models.GroupMember{}).
					Where("user_id = ? AND status = 'ACTIVE'", uid).
					Pluck("group_id", &activeGroupIDs)

				if len(activeGroupIDs) > 0 {
					type ConflictInfo struct {
						GroupName string
						UserName  string
					}
					var conflict ConflictInfo
					cErr := db.Table("group_members").
						Joins("JOIN users ON users.id = group_members.user_id").
						Joins("JOIN groups ON groups.id = group_members.group_id").
						Where("group_members.group_id IN ? AND group_members.status = 'ACTIVE' AND group_members.user_id != ? AND users.kit_number = ?", activeGroupIDs, uid, newKit).
						Select("groups.name as group_name, users.name as user_name").
						First(&conflict).Error
					if cErr == nil {
						c.JSON(409, err(fmt.Sprintf("jersey number #%d is already taken by %s in %s", newKit, conflict.UserName, conflict.GroupName)))
						return
					}
				}
			}
			updates["kit_number"] = newKit
		}
		if in.FavouriteClub != nil {
			updates["favourite_club"] = strings.TrimSpace(*in.FavouriteClub)
		}
		if in.FavouritePlayer != nil {
			updates["favourite_player"] = strings.TrimSpace(*in.FavouritePlayer)
		}
		if len(updates) > 0 {
			if e := db.Model(&u).Updates(updates).Error; e != nil {
				c.JSON(500, err("failed to update profile"))
				return
			}
			if oldKeyToDelete != "" {
				go func(b, k string) {
					delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if delErr := imgStorage.DeleteObject(delCtx, b, k); delErr != nil {
						log.Printf("[Storage] Failed to delete previous profile photo %s/%s: %v", b, k, delErr)
					} else {
						log.Printf("[Storage] Deleted previous profile photo %s/%s", b, k)
					}
				}(oldBucketToDelete, oldKeyToDelete)
			}
			if e := db.First(&u, uid).Error; e != nil {
				c.JSON(500, err("failed to reload profile"))
				return
			}
		}
		u.CareerStats = computeCareerStats(&u)
		c.JSON(200, gin.H{"success": true, "data": u})
	})
	sec.POST("/users/device-token", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		var in struct {
			Token    string `json:"token"`
			Platform string `json:"platform"`
		}
		if e := c.BindJSON(&in); e != nil || strings.TrimSpace(in.Token) == "" {
			c.JSON(400, err("valid token is required"))
			return
		}
		if in.Platform == "" {
			in.Platform = "android"
		}
		if e := pushService.RegisterToken(uid, in.Token, in.Platform); e != nil {
			c.JSON(500, err("failed to register device token"))
			return
		}
		c.JSON(200, gin.H{"success": true, "message": "device token registered"})
	})
	sec.DELETE("/users/device-token", func(c *gin.Context) {
		var in struct {
			Token string `json:"token"`
		}
		_ = c.BindJSON(&in)
		if strings.TrimSpace(in.Token) != "" {
			_ = pushService.DeleteToken(in.Token)
		}
		c.JSON(200, gin.H{"success": true, "message": "device token removed"})
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
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		var g models.Group
		if e := db.First(&g, gid).Error; e != nil {
			c.JSON(404, err("group not found"))
			return
		}
		var unreadCount int64
		db.Model(&models.ChatMessage{}).
			Where("group_id = ? AND sender_id != ?", gid, uid).
			Where("id NOT IN (SELECT message_id FROM chat_message_reads WHERE user_id = ? AND group_id = ?)", uid, gid).
			Count(&unreadCount)
		c.JSON(200, gin.H{"success": true, "data": g, "unread_count": unreadCount})
	})
	sec.PATCH("/groups/:id", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !isGroupAdmin(db, gid, uid) {
			c.JSON(403, err("admin access required"))
			return
		}
		var g models.Group
		if e := db.First(&g, gid).Error; e != nil {
			c.JSON(404, err("group not found"))
			return
		}
		var in struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			City        *string `json:"city"`
			LogoURL     *string `json:"logo_url"`
			LogoBucket  *string `json:"logo_bucket"`
			LogoKey     *string `json:"logo_key"`
		}
		if c.ShouldBindJSON(&in) != nil {
			c.JSON(400, err("invalid request"))
			return
		}
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			g.Name = strings.TrimSpace(*in.Name)
		}
		if in.Description != nil {
			g.Description = strings.TrimSpace(*in.Description)
		}
		if in.City != nil {
			g.City = strings.TrimSpace(*in.City)
		}
		var oldLogoKey, oldLogoBucket string
		if in.LogoURL != nil || in.LogoKey != nil {
			var newBucket, newKey, newCleanURL string
			if in.LogoKey != nil && *in.LogoKey != "" {
				newKey = strings.TrimSpace(*in.LogoKey)
				newBucket = imgStorage.BucketName()
				if in.LogoBucket != nil && *in.LogoBucket != "" {
					newBucket = strings.TrimSpace(*in.LogoBucket)
				}
				newCleanURL = newKey
			} else if in.LogoURL != nil {
				raw := strings.TrimSpace(*in.LogoURL)
				if raw != "" {
					newBucket, newKey = storage.ExtractBucketAndKey(raw, imgStorage.BucketName())
					newCleanURL = cleanStoredURL(raw)
				}
			}

			oldKey := g.LogoKey
			oldBucket := g.LogoBucket
			if oldKey == "" && g.LogoURL != "" {
				oldBucket, oldKey = storage.ExtractBucketAndKey(g.LogoURL, imgStorage.BucketName())
			}
			if oldBucket == "" {
				oldBucket = imgStorage.BucketName()
			}

			if oldKey != "" && oldKey != newKey {
				oldLogoKey = oldKey
				oldLogoBucket = oldBucket
			}

			g.LogoBucket = newBucket
			g.LogoKey = newKey
			g.LogoURL = newCleanURL
		}
		if e := db.Save(&g).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		if oldLogoKey != "" {
			go func(b, k string) {
				delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if delErr := imgStorage.DeleteObject(delCtx, b, k); delErr != nil {
					log.Printf("[Storage] Failed to delete previous squad logo %s/%s: %v", b, k, delErr)
				} else {
					log.Printf("[Storage] Deleted previous squad logo %s/%s", b, k)
				}
			}(oldLogoBucket, oldLogoKey)
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
			type UserCareerAgg struct {
				UserID      uuid.UUID `gorm:"column:user_id"`
				Matches     int       `gorm:"column:matches"`
				Wins        int       `gorm:"column:wins"`
				Draws       int       `gorm:"column:draws"`
				Losses      int       `gorm:"column:losses"`
				Goals       int       `gorm:"column:goals"`
				Assists     int       `gorm:"column:assists"`
				MVP         int       `gorm:"column:mvp"`
				CleanSheets int       `gorm:"column:clean_sheets"`
			}
			var aggs []UserCareerAgg
			db.Table("player_statistics").
				Select("user_id, SUM(matches) as matches, SUM(wins) as wins, SUM(draws) as draws, SUM(losses) as losses, SUM(goals) as goals, SUM(assists) as assists, SUM(mvp) as mvp, SUM(clean_sheets) as clean_sheets").
				Where("user_id IN ?", ids).
				Group("user_id").
				Scan(&aggs)
			aggMap := make(map[uuid.UUID]UserCareerAgg)
			for _, a := range aggs {
				aggMap[a.UserID] = a
			}
			for i := range us {
				if a, ok := aggMap[us[i].ID]; ok {
					winRate := 0.0
					if a.Matches > 0 {
						winRate = math.Round((float64(a.Wins)/float64(a.Matches))*1000) / 10
					}
					us[i].CareerStats = &models.CareerStats{
						Matches:     a.Matches,
						Wins:        a.Wins,
						Draws:       a.Draws,
						Losses:      a.Losses,
						Goals:       a.Goals,
						Assists:     a.Assists,
						MVP:         a.MVP,
						CleanSheets: a.CleanSheets,
						WinRate:     winRate,
					}
				} else {
					us[i].CareerStats = &models.CareerStats{}
				}
			}
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

		var g models.Group
		if db.First(&g, gid).Error != nil {
			c.JSON(404, err("group not found"))
			return
		}

		// Only the group OWNER can promote to ADMIN or demote an existing ADMIN
		if targetMember.Role == "ADMIN" || newRole == "ADMIN" {
			if g.OwnerID != actorUID {
				c.JSON(403, err("only the group owner can promote or demote admins"))
				return
			}
		}

		targetMember.Role = newRole
		if e := db.Save(&targetMember).Error; e != nil {
			c.JSON(500, err("failed to update member role"))
			return
		}

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
	sec.DELETE("/groups/:id/members/:userId", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		currentUID := mustUUID(auth.UserID(c))
		targetUID := mustUUID(c.Param("userId"))

		var targetMember models.GroupMember
		if e := db.Where("group_id = ? AND user_id = ? AND status = 'ACTIVE'", gid, targetUID).First(&targetMember).Error; e != nil {
			c.JSON(404, err("member not found in group"))
			return
		}

		var g models.Group
		if e := db.First(&g, gid).Error; e != nil {
			c.JSON(404, err("group not found"))
			return
		}

		isSelf := currentUID == targetUID
		if isSelf {
			if g.OwnerID == currentUID {
				c.JSON(400, err("squad owner cannot leave the squad. Transfer ownership or delete the squad."))
				return
			}
			targetMember.Status = "LEFT"
			if e := db.Save(&targetMember).Error; e != nil {
				c.JSON(500, err(e.Error()))
				return
			}
			notifyGroupMembers(db, gid, &currentUID, "MEMBER_LEFT", "Member Left", "A member has left the squad.", "GROUP", &gid)
			c.JSON(200, gin.H{"success": true, "message": "left squad successfully"})
			return
		}

		if !isGroupAdmin(db, gid, currentUID) {
			c.JSON(403, err("admin access required to remove members"))
			return
		}
		if targetUID == g.OwnerID {
			c.JSON(400, err("cannot remove the squad owner"))
			return
		}
		if targetMember.Role == "ADMIN" && currentUID != g.OwnerID {
			c.JSON(403, err("only the squad owner can remove an admin"))
			return
		}

		targetMember.Status = "REMOVED"
		if e := db.Save(&targetMember).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}

		n := models.Notification{
			UserID:     targetUID,
			GroupID:    &gid,
			Type:       "MEMBER_REMOVED",
			Title:      "Removed from Squad",
			Message:    "You have been removed from " + g.Name + ".",
			EntityType: "GROUP",
			EntityID:   &gid,
		}
		_ = db.Create(&n).Error

		c.JSON(200, gin.H{"success": true, "message": "member removed successfully"})
	})
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
	sec.POST("/groups/:id/join-requests/:requestId/reject", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		var request models.GroupJoinRequest
		if e := db.First(&request, mustUUID(c.Param("requestId"))).Error; e != nil || request.GroupID != gid {
			c.JSON(404, err("join request not found"))
			return
		}
		r, e := gs.Reject(mustUUID(c.Param("requestId")), mustUUID(auth.UserID(c)))
		if e != nil {
			c.JSON(403, err(e.Error()))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": r})
	})
	sec.GET("/groups/:id/ratings/:userId", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		avg, n, e := rs.Average(gid, mustUUID(c.Param("userId")))
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
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		var msgs []models.ChatMessage
		if e := db.Where("group_id=?", gid).Order("created_at ASC").Limit(100).Find(&msgs).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		if len(msgs) == 0 {
			c.JSON(200, gin.H{"success": true, "data": []any{}})
			return
		}

		var msgIDs []uuid.UUID
		for _, m := range msgs {
			msgIDs = append(msgIDs, m.ID)
		}
		var reads []models.ChatMessageRead
		db.Where("message_id IN ?", msgIDs).Find(&reads)
		readsMap := make(map[uuid.UUID][]uuid.UUID)
		for _, r := range reads {
			readsMap[r.MessageID] = append(readsMap[r.MessageID], r.UserID)
		}

		type chatMsgResponse struct {
			models.ChatMessage
			SeenCount int         `json:"seen_count"`
			SeenByIDs []uuid.UUID `json:"seen_by_ids"`
		}
		out := make([]chatMsgResponse, len(msgs))
		for i, m := range msgs {
			userIDs := readsMap[m.ID]
			if userIDs == nil {
				userIDs = []uuid.UUID{}
			}
			out[i] = chatMsgResponse{
				ChatMessage: m,
				SeenCount:   len(userIDs),
				SeenByIDs:   userIDs,
			}
		}
		c.JSON(200, gin.H{"success": true, "data": out})
	})
	sec.POST("/groups/:id/chat/read", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		var in struct {
			MessageIDs []string `json:"message_ids"`
		}
		_ = c.BindJSON(&in)

		var msgs []models.ChatMessage
		if len(in.MessageIDs) > 0 {
			var ids []uuid.UUID
			for _, sid := range in.MessageIDs {
				if id, err := uuid.Parse(sid); err == nil {
					ids = append(ids, id)
				}
			}
			if len(ids) > 0 {
				db.Where("group_id=? AND id IN ? AND sender_id != ?", gid, ids, uid).Find(&msgs)
			}
		} else {
			db.Where("group_id=? AND sender_id != ?", gid, uid).Order("created_at DESC").Limit(100).Find(&msgs)
		}

		if len(msgs) == 0 {
			c.JSON(200, gin.H{"success": true, "data": gin.H{"marked_count": 0}})
			return
		}

		now := time.Now().UTC()
		var readIDs []uuid.UUID
		for _, m := range msgs {
			var existing models.ChatMessageRead
			if db.Where("message_id=? AND user_id=?", m.ID, uid).First(&existing).Error != nil {
				r := models.ChatMessageRead{
					MessageID: m.ID,
					UserID:    uid,
					GroupID:   gid,
					ReadAt:    now,
				}
				if db.Create(&r).Error == nil {
					readIDs = append(readIDs, m.ID)
				}
			}
		}

		if len(readIDs) > 0 {
			hub.Broadcast(gid.String(), ws.Event{
				Type: "MESSAGES_READ",
				Data: gin.H{
					"group_id":    gid,
					"user_id":     uid,
					"message_ids": readIDs,
					"read_at":     now,
				},
			})
		}

		c.JSON(200, gin.H{"success": true, "data": gin.H{"marked_count": len(readIDs)}})
	})
	sec.GET("/groups/:id/chat/unread", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		var count int64
		db.Model(&models.ChatMessage{}).
			Where("group_id = ? AND sender_id != ?", gid, uid).
			Where("id NOT IN (SELECT message_id FROM chat_message_reads WHERE user_id = ? AND group_id = ?)", uid, gid).
			Count(&count)
		c.JSON(200, gin.H{"success": true, "data": gin.H{"unread_count": count}})
	})
	sec.POST("/groups/:id/chat/typing", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
		var in struct {
			IsTyping bool `json:"is_typing"`
		}
		if e := c.ShouldBindJSON(&in); e != nil {
			c.JSON(400, err("invalid input"))
			return
		}
		var user models.User
		db.Select("id, name").First(&user, uid)
		hub.Broadcast(gid.String(), ws.Event{
			Type: "USER_TYPING",
			Data: gin.H{
				"user_id":   uid.String(),
				"user_name": user.Name,
				"is_typing": in.IsTyping,
			},
		})
		c.JSON(200, gin.H{"success": true})
	})
	getSeenHandler := func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		mid := mustUUID(c.Param("message_id"))
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}

		var msg models.ChatMessage
		if db.Where("id=? AND group_id=?", mid, gid).First(&msg).Error != nil {
			c.JSON(404, err("message not found"))
			return
		}

		var reads []models.ChatMessageRead
		db.Where("message_id=?", mid).Order("read_at ASC").Find(&reads)

		type readReceiptItem struct {
			User   models.User `json:"user"`
			ReadAt time.Time   `json:"read_at"`
		}

		var members []models.GroupMember
		db.Where("group_id=? AND status='ACTIVE'", gid).Find(&members)

		// Batch lookup all referenced user profiles in a single query
		userIDs := make([]uuid.UUID, 0, len(reads)+len(members))
		for _, r := range reads {
			userIDs = append(userIDs, r.UserID)
		}
		for _, mb := range members {
			userIDs = append(userIDs, mb.UserID)
		}
		userMap := make(map[uuid.UUID]models.User)
		if len(userIDs) > 0 {
			var users []models.User
			db.Where("id IN ?", userIDs).Find(&users)
			for _, u := range users {
				userMap[u.ID] = u
			}
		}

		var seenBy []readReceiptItem
		readUserMap := make(map[uuid.UUID]bool)
		for _, r := range reads {
			if u, ok := userMap[r.UserID]; ok {
				seenBy = append(seenBy, readReceiptItem{
					User:   u,
					ReadAt: r.ReadAt,
				})
				readUserMap[r.UserID] = true
			}
		}

		var unseenBy []models.User
		for _, mb := range members {
			if mb.UserID == msg.SenderID {
				continue
			}
			if !readUserMap[mb.UserID] {
				if u, ok := userMap[mb.UserID]; ok {
					unseenBy = append(unseenBy, u)
				}
			}
		}

		if seenBy == nil {
			seenBy = []readReceiptItem{}
		}
		if unseenBy == nil {
			unseenBy = []models.User{}
		}

		c.JSON(200, gin.H{
			"success": true,
			"data": gin.H{
				"message_id": mid,
				"seen_by":    seenBy,
				"unseen_by":  unseenBy,
			},
		})
	}
	sec.GET("/groups/:id/chat/messages/:message_id/seen", getSeenHandler)
	sec.GET("/groups/:id/chat/:message_id/seen", getSeenHandler)
	sec.POST("/groups/:id/chat", func(c *gin.Context) {
		gid := mustUUID(c.Param("id"))
		var in struct{ Content string }
		if c.BindJSON(&in) != nil || strings.TrimSpace(in.Content) == "" {
			c.JSON(400, err("message cannot be empty"))
			return
		}
		if len(in.Content) > 2000 {
			c.JSON(400, err("message exceeds maximum limit of 2000 characters"))
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

		// Detect mentions of squad members and create in-app notifications
		go func(content string, senderUID uuid.UUID, groupID uuid.UUID) {
			var members []models.GroupMember
			db.Where("group_id=?", groupID).Find(&members)
			var group models.Group
			if err := db.First(&group, groupID).Error; err != nil {
				log.Printf("[Chat Mention] Failed to find group %s: %v", groupID, err)
				return
			}
			var sender models.User
			if err := db.First(&sender, senderUID).Error; err != nil {
				log.Printf("[Chat Mention] Failed to find sender %s: %v", senderUID, err)
				return
			}

			var targetUserIDs []uuid.UUID
			for _, mb := range members {
				if mb.UserID != senderUID {
					targetUserIDs = append(targetUserIDs, mb.UserID)
				}
			}
			if len(targetUserIDs) == 0 {
				return
			}

			var targets []models.User
			db.Where("id IN ?", targetUserIDs).Find(&targets)

			contentLower := strings.ToLower(content)
			hasAllMention := strings.Contains(contentLower, "@all") || strings.Contains(contentLower, "@everyone") || strings.Contains(contentLower, "@squad")
			matchedCount := 0

			for _, target := range targets {
				targetName := strings.TrimSpace(target.Name)
				targetLower := strings.ToLower(targetName)
				nameParts := strings.Fields(targetLower)

				emailPrefix := ""
				if atIdx := strings.Index(target.Email, "@"); atIdx > 0 {
					emailPrefix = strings.ToLower(target.Email[:atIdx])
				}

				hasFullName := targetLower != "" && strings.Contains(contentLower, "@"+targetLower)
				hasFirstName := len(nameParts) > 0 && len(nameParts[0]) >= 2 && strings.Contains(contentLower, "@"+nameParts[0])
				hasEmailPrefix := emailPrefix != "" && len(emailPrefix) >= 2 && strings.Contains(contentLower, "@"+emailPrefix)

				if hasAllMention || hasFullName || hasFirstName || hasEmailPrefix {
					matchedCount++
					log.Printf("[Chat Mention] Matched user %s (%s) in group %s from sender %s", target.ID, target.Name, group.Name, sender.Name)
					snippet := content
					if len(snippet) > 80 {
						snippet = snippet[:77] + "..."
					}
					msg := fmt.Sprintf("%s tagged you in %s: \"%s\"", sender.Name, group.Name, snippet)
					notif := models.Notification{
						UserID:     target.ID,
						GroupID:    &groupID,
						Type:       "CHAT_MENTION",
						Title:      "Mentioned in squad chat",
						Message:    msg,
						EntityType: "GROUP_CHAT",
						EntityID:   &groupID,
					}
					if err := db.Create(&notif).Error; err != nil {
						log.Printf("[Chat Mention] Failed to create notification for %s: %v", target.ID, err)
					}
				}
			}
			if matchedCount == 0 {
				log.Printf("[Chat Mention] No users matched mention query in message: %q", content)
			}
		}(in.Content, uid, gid)

		c.JSON(201, gin.H{"success": true, "data": m})
	})
	sec.GET("/presence/online", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"success": true,
			"data": gin.H{
				"online_user_ids": presence.OnlineUserIDs(),
			},
		})
	})
	sec.GET("/ws/presence", func(c *gin.Context) {
		uid := mustUUID(auth.UserID(c))
		if uid == uuid.Nil {
			c.AbortWithStatus(401)
			return
		}
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, e := up.Upgrade(c.Writer, c.Request, nil)
		if e != nil {
			return
		}
		cl := &ws.Client{Conn: conn, Key: "presence", UserID: uid.String()}
		hub.Add(cl)
		defer hub.Remove(cl)

		justCameOnline := presence.Connect(uid.String())
		if justCameOnline {
			hub.Broadcast("presence", ws.Event{
				Type: "USER_ONLINE",
				Data: gin.H{"user_id": uid.String()},
			})
		}

		// Initial sync of all currently active online users
		syncData, _ := json.Marshal(ws.Event{
			Type: "PRESENCE_SYNC",
			Data: gin.H{"online_user_ids": presence.OnlineUserIDs()},
		})
		cl.Mu.Lock()
		_ = conn.WriteMessage(websocket.TextMessage, syncData)
		cl.Mu.Unlock()

		defer func() {
			justWentOffline := presence.Disconnect(uid.String())
			if justWentOffline {
				hub.Broadcast("presence", ws.Event{
					Type: "USER_OFFLINE",
					Data: gin.H{"user_id": uid.String()},
				})
			}
		}()

		for {
			if _, _, e := conn.ReadMessage(); e != nil {
				return
			}
		}
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

		justCameOnline := presence.Connect(uid.String())
		if justCameOnline {
			hub.Broadcast("presence", ws.Event{
				Type: "USER_ONLINE",
				Data: gin.H{"user_id": uid.String()},
			})
		}
		defer func() {
			justWentOffline := presence.Disconnect(uid.String())
			if justWentOffline {
				hub.Broadcast("presence", ws.Event{
					Type: "USER_OFFLINE",
					Data: gin.H{"user_id": uid.String()},
				})
			}
		}()

		for {
			_, raw, e := conn.ReadMessage()
			if e != nil {
				return
			}
			var in struct {
				Type     string `json:"type"`
				IsTyping bool   `json:"is_typing"`
			}
			if json.Unmarshal(raw, &in) == nil && (in.Type == "TYPING" || in.Type == "TYPING_START" || in.Type == "TYPING_STOP") {
				isTyping := in.IsTyping || in.Type == "TYPING_START"
				if in.Type == "TYPING_STOP" {
					isTyping = false
				}
				var user models.User
				db.Select("id, name").First(&user, uid)
				hub.Broadcast(gid.String(), ws.Event{
					Type: "USER_TYPING",
					Data: gin.H{
						"user_id":   uid.String(),
						"user_name": user.Name,
						"is_typing": isTyping,
					},
				})
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
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
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
			Venue           string `json:"venue"`
			VenueMapURL     string `json:"venue_map_url"`
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
		if e := db.First(&g, gid).Error; e != nil {
			c.JSON(404, err("group not found"))
			return
		}
		t, e := time.Parse(time.RFC3339, in.ScheduledAt)
		if e != nil {
			t = time.Now().Add(24 * time.Hour)
		}
		venueName, mapURL := resolveVenue(in.Venue, in.VenueMapURL)
		m := models.Match{
			GroupID:         gid,
			SportID:         g.SportID,
			Name:            in.Name,
			ScheduledAt:     t,
			Format:          in.Format,
			DurationMinutes: in.DurationMinutes,
			TeamCount:       defaultInt(in.TeamCount, 2),
			PlayersPerTeam:  in.PlayersPerTeam,
			MaxPlayers:      in.MaxPlayers,
			Notes:           in.Notes,
			Venue:           venueName,
			VenueMapURL:     mapURL,
			Status:          "UPCOMING",
		}
		if in.VenueID != "" {
			id := mustUUID(in.VenueID)
			m.VenueID = &id
			if m.Venue == "" || m.VenueMapURL == "" {
				var v models.Venue
				if db.First(&v, id).Error == nil {
					if m.Venue == "" {
						m.Venue = v.Name
					}
					if m.VenueMapURL == "" {
						m.VenueMapURL = v.GoogleMapsURL
					}
				}
			}
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
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
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
	sec.PUT("/matches/:id", func(c *gin.Context) {
		m := c.MustGet("match").(models.Match)
		uid := mustUUID(auth.UserID(c))
		if !mustAdmin(db, m.GroupID, uid) {
			c.JSON(403, err("admin only"))
			return
		}
		if m.Status != "UPCOMING" {
			c.JSON(400, err("only upcoming matches can be edited"))
			return
		}
		var in struct {
			Name            string `json:"name"`
			ScheduledAt     string `json:"scheduled_at"`
			VenueID         string `json:"venue_id"`
			Venue           string `json:"venue"`
			VenueMapURL     string `json:"venue_map_url"`
			Format          string `json:"format"`
			Notes           string `json:"notes"`
			DurationMinutes int    `json:"duration_minutes"`
			TeamCount       int    `json:"team_count"`
			PlayersPerTeam  int    `json:"players_per_team"`
			MaxPlayers      int    `json:"max_players"`
		}
		if c.BindJSON(&in) != nil || strings.TrimSpace(in.Name) == "" {
			c.JSON(400, err("match name required"))
			return
		}
		if in.ScheduledAt != "" {
			if t, err := time.Parse(time.RFC3339, in.ScheduledAt); err == nil {
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
		m.Notes = in.Notes
		venueName, mapURL := resolveVenue(in.Venue, in.VenueMapURL)
		m.Venue = venueName
		m.VenueMapURL = mapURL
		if in.VenueID != "" {
			vid := mustUUID(in.VenueID)
			m.VenueID = &vid
			if m.Venue == "" || m.VenueMapURL == "" {
				var v models.Venue
				if db.First(&v, vid).Error == nil {
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
		if e := db.Save(&m).Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": m})
	})
	sec.DELETE("/matches/:id", func(c *gin.Context) {
		m := c.MustGet("match").(models.Match)
		uid := mustUUID(auth.UserID(c))
		if !mustAdmin(db, m.GroupID, uid) {
			c.JSON(403, err("admin only"))
			return
		}
		if m.Status != "UPCOMING" {
			c.JSON(400, err("only upcoming matches can be deleted"))
			return
		}
		txErr := db.Transaction(func(tx *gorm.DB) error {
			var teams []models.Team
			if err := tx.Where("match_id = ?", m.ID).Find(&teams).Error; err != nil {
				return err
			}
			teamIDs := make([]uuid.UUID, 0, len(teams))
			for _, t := range teams {
				teamIDs = append(teamIDs, t.ID)
			}
			if len(teamIDs) > 0 {
				if err := tx.Where("team_id IN ?", teamIDs).Delete(&models.TeamMember{}).Error; err != nil {
					return err
				}
				if err := tx.Where("id IN ?", teamIDs).Delete(&models.Team{}).Error; err != nil {
					return err
				}
			}
			if err := tx.Where("match_id = ?", m.ID).Delete(&models.Attendance{}).Error; err != nil {
				return err
			}
			if err := tx.Where("match_id = ?", m.ID).Delete(&models.MatchEvent{}).Error; err != nil {
				return err
			}
			if err := tx.Where("match_id = ?", m.ID).Delete(&models.MatchResult{}).Error; err != nil {
				return err
			}
			return tx.Delete(&m).Error
		})
		if txErr != nil {
			c.JSON(500, err("could not delete match"))
			return
		}
		c.JSON(200, gin.H{"success": true, "message": "match deleted successfully"})
	})
	sec.POST("/matches/:id/attendance", func(c *gin.Context) {
		m := c.MustGet("match").(models.Match)
		if m.Status != "UPCOMING" {
			c.JSON(400, err("attendance marking is closed for this match"))
			return
		}
		mid := m.ID
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
		if st != "GOING" {
			// If member is no longer GOING, remove them from any generated squads for this upcoming match
			var match models.Match
			if err := db.First(&match, mid).Error; err == nil && match.Status == "UPCOMING" {
				db.Exec(`
					DELETE FROM team_members 
					WHERE user_id = ? AND team_id IN (
						SELECT id FROM teams WHERE match_id = ?
					)
				`, uid, mid)
			}
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
		if m.Status != "UPCOMING" {
			c.JSON(400, err("squads can only be generated for upcoming matches"))
			return
		}

		teamCount := defaultInt(m.TeamCount, 2)

		// 1. Query attendances with status = 'GOING' for this match,
		// ensuring attendees are active members of the group.
		var ax []models.Attendance
		if e := db.Joins("JOIN group_members gm ON gm.user_id = attendances.user_id AND gm.group_id = ? AND gm.status = 'ACTIVE'", m.GroupID).
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
		db.Where("id IN ?", ids).Find(&us)
		userMap := make(map[uuid.UUID]models.User, len(us))
		for _, u := range us {
			userMap[u.ID] = u
		}

		ratings := map[uuid.UUID]float64{}
		for _, u := range us {
			ratings[u.ID], _, _ = rs.Average(m.GroupID, u.ID)
		}

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
					TeamID:       tm.ID,
					UserID:       x.UserID,
					PositionName: normalizePosition(x.Position),
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

		notifyGroupMembers(db, m.GroupID, nil, "SQUADS_GENERATED", "Balanced Squads Generated ⚔️", fmt.Sprintf("Squads have been generated for '%s'. Check your team assignment!", m.Name), "MATCH", &mid)

		c.JSON(200, gin.H{
			"success": true,
			"data":    out,
		})
	})
	loadMatchTeams := func(d *gorm.DB, matchID uuid.UUID) ([]gin.H, error) {
		var ts []models.Team
		if e := d.Where("match_id=?", matchID).Order("created_at ASC, id ASC").Find(&ts).Error; e != nil {
			return nil, e
		}
		type memberOut struct {
			models.TeamMember
			User *models.User `json:"user,omitempty"`
		}
		out := make([]gin.H, 0, len(ts))
		for _, t := range ts {
			var mem []models.TeamMember
			d.Where("team_id=?", t.ID).Find(&mem)
			uids := make([]uuid.UUID, 0, len(mem))
			for _, m := range mem {
				uids = append(uids, m.UserID)
			}
			var users []models.User
			if len(uids) > 0 {
				d.Where("id IN ?", uids).Find(&users)
			}
			uMap := make(map[uuid.UUID]models.User, len(users))
			for _, u := range users {
				uMap[u.ID] = u
			}
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

	recalcTeamStrength := func(tx *gorm.DB, teamID, groupID uuid.UUID) error {
		var members []models.TeamMember
		if err := tx.Where("team_id = ?", teamID).Find(&members).Error; err != nil {
			return err
		}
		if len(members) == 0 {
			return tx.Model(&models.Team{}).Where("id = ?", teamID).Update("strength", 0.0).Error
		}
		var totalRating float64
		for _, m := range members {
			r, _, _ := rs.Average(groupID, m.UserID)
			if r == 0 {
				r = 5.0
			}
			totalRating += r
		}
		strength := totalRating / float64(len(members))
		return tx.Model(&models.Team{}).Where("id = ?", teamID).Update("strength", strength).Error
	}

	sec.GET("/matches/:id/teams", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		out, errLoad := loadMatchTeams(db, mid)
		if errLoad != nil {
			c.JSON(500, err("could not load teams"))
			return
		}
		c.JSON(200, gin.H{"success": true, "data": out})
	})

	sec.POST("/matches/:id/teams/move-player", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		var m models.Match
		if e := db.First(&m, mid).Error; e != nil {
			c.JSON(404, err("match not found"))
			return
		}
		if !mustAdmin(db, m.GroupID, mustUUID(auth.UserID(c))) {
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
		if e := db.Where("id = ? AND match_id = ?", targetTeamUID, mid).First(&targetTeam).Error; e != nil {
			c.JSON(404, err("target team not found for this match"))
			return
		}

		var currentMember models.TeamMember
		if e := db.Joins("JOIN teams ON teams.id = team_members.team_id").
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

		tx := db.Begin()
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

		_ = recalcTeamStrength(tx, oldTeamID, m.GroupID)
		_ = recalcTeamStrength(tx, targetTeamUID, m.GroupID)

		if e := tx.Commit().Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}

		out, errLoad := loadMatchTeams(db, mid)
		if errLoad != nil {
			c.JSON(500, err("could not load updated teams"))
			return
		}

		c.JSON(200, gin.H{
			"success": true,
			"data":    out,
			"message": "Player moved successfully",
		})
	})

	sec.POST("/matches/:id/teams/swap-players", func(c *gin.Context) {
		mid := mustUUID(c.Param("id"))
		var m models.Match
		if e := db.First(&m, mid).Error; e != nil {
			c.JSON(404, err("match not found"))
			return
		}
		if !mustAdmin(db, m.GroupID, mustUUID(auth.UserID(c))) {
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
		if e := db.Joins("JOIN teams ON teams.id = team_members.team_id").
			Where("teams.match_id = ? AND team_members.user_id = ?", mid, p1UID).
			First(&mem1).Error; e != nil {
			c.JSON(404, err("player 1 is not assigned to any team in this match"))
			return
		}

		var mem2 models.TeamMember
		if e := db.Joins("JOIN teams ON teams.id = team_members.team_id").
			Where("teams.match_id = ? AND team_members.user_id = ?", mid, p2UID).
			First(&mem2).Error; e != nil {
			c.JSON(404, err("player 2 is not assigned to any team in this match"))
			return
		}

		if mem1.TeamID == mem2.TeamID {
			c.JSON(400, err("both players are already on the same team"))
			return
		}

		tx := db.Begin()
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

		_ = recalcTeamStrength(tx, team1ID, m.GroupID)
		_ = recalcTeamStrength(tx, team2ID, m.GroupID)

		if e := tx.Commit().Error; e != nil {
			c.JSON(500, err(e.Error()))
			return
		}

		out, errLoad := loadMatchTeams(db, mid)
		if errLoad != nil {
			c.JSON(500, err("could not load updated teams"))
			return
		}

		c.JSON(200, gin.H{
			"success": true,
			"data":    out,
			"message": "Players swapped successfully",
		})
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

		teamCount := len(lineups)
		layoutVs := "* Large centered \"VS\"\n"
		if teamCount > 2 {
			layoutVs = fmt.Sprintf("* Multi-team triangular or round-robin division showing all %d teams prominently with VS dividers between them\n", teamCount)
		}

		prompt := fmt.Sprintf(
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

		// Persist poster image to storage (Amazon S3 or fallback)
		posterFilename := fmt.Sprintf("posters/%s.png", mid.String())
		posterURL, uploadErr := imgStorage.Upload(c.Request.Context(), posterFilename, imgBytes, "image/png")
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

		// Keep local disk copy as fallback for /api/matches/:id/poster-image
		uploadsDir := filepath.Join("uploads", "posters")
		_ = os.MkdirAll(uploadsDir, 0755)
		_ = os.WriteFile(filepath.Join(uploadsDir, fmt.Sprintf("%s.png", mid.String())), imgBytes, 0644)

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
		if e := db.First(&m, mid).Error; e != nil {
			c.JSON(404, err("match not found"))
			return
		}
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
		uid := mustUUID(auth.UserID(c))
		if !mustMember(db, gid, uid) {
			c.JSON(403, err("not a group member"))
			return
		}
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
	if db.Where("name=?", "Football").First(&s).Error != nil {
		s = models.Sport{Name: "Football", Icon: "⚽", Active: true}
		if err := db.Create(&s).Error; err != nil {
			log.Printf("[Seed] Failed to create sport Football: %v", err)
			return
		}
	}

	desired := []struct {
		Name string
		Code string
	}{
		{Name: "Goalkeeper", Code: "GK"},
		{Name: "Defender", Code: "DEF"},
		{Name: "Midfielder", Code: "MID"},
		{Name: "Striker", Code: "ST"},
	}

	validCodes := []string{"GK", "DEF", "MID", "ST"}
	// Keep only the 4 canonical positions for Football
	db.Where("sport_id = ? AND code NOT IN ?", s.ID, validCodes).Delete(&models.SportPosition{})

	for i, p := range desired {
		var sp models.SportPosition
		if err := db.Where("sport_id = ? AND code = ?", s.ID, p.Code).First(&sp).Error; err != nil {
			db.Create(&models.SportPosition{
				SportID:   s.ID,
				Name:      p.Name,
				Code:      p.Code,
				SortOrder: i,
			})
		} else {
			db.Model(&sp).Updates(map[string]interface{}{
				"name":       p.Name,
				"sort_order": i,
			})
		}
	}

	// Migrate legacy position codes in users table
	db.Model(&models.User{}).Where("UPPER(TRIM(position)) IN ?", []string{"MF", "MIDFIELDER", "CM", "CAM", "CDM", "LM", "RM"}).Update("position", "MID")
	db.Model(&models.User{}).Where("UPPER(TRIM(position)) IN ?", []string{"CB", "FB", "DEFENDER", "LB", "RB", "CENTRE BACK", "FULL BACK"}).Update("position", "DEF")
	db.Model(&models.User{}).Where("UPPER(TRIM(position)) IN ?", []string{"WG", "FWD", "FORWARD", "STRIKER", "CF", "ATTACKER", "LW", "RW", "WINGER"}).Update("position", "ST")
	db.Model(&models.User{}).Where("UPPER(TRIM(position)) IN ?", []string{"GOALKEEPER", "GOALIE", "KEEPER"}).Update("position", "GK")
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

func resolveVenue(venue, mapURL string) (string, string) {
	venue = strings.TrimSpace(venue)
	mapURL = strings.TrimSpace(mapURL)

	isURL := func(s string) bool {
		lower := strings.ToLower(s)
		return strings.HasPrefix(lower, "http://") ||
			strings.HasPrefix(lower, "https://") ||
			strings.HasPrefix(lower, "maps.app.goo.gl") ||
			strings.HasPrefix(lower, "goo.gl/maps") ||
			strings.HasPrefix(lower, "maps.google.") ||
			strings.HasPrefix(lower, "www.google.com/maps")
	}

	normalizeURL := func(s string) string {
		s = strings.TrimSpace(s)
		lower := strings.ToLower(s)
		if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
			return "https://" + s
		}
		return s
	}

	if mapURL != "" && isURL(mapURL) {
		mapURL = normalizeURL(mapURL)
	}

	if isURL(venue) {
		if mapURL == "" {
			mapURL = normalizeURL(venue)
		}
		return venue, mapURL
	}

	words := strings.Fields(venue)
	var textWords []string
	for _, w := range words {
		if isURL(w) {
			if mapURL == "" {
				mapURL = normalizeURL(w)
			}
		} else {
			textWords = append(textWords, w)
		}
	}

	if len(textWords) > 0 && mapURL != "" && len(textWords) < len(words) {
		venue = strings.Join(textWords, " ")
		venue = strings.Trim(venue, " -:,|")
	}

	return venue, mapURL
}

func cleanStoredURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Scheme != "" {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	return raw
}

func normalizePosition(pos string) string {
	clean := strings.ToUpper(strings.TrimSpace(pos))
	switch {
	case clean == "GK" || strings.Contains(clean, "GOAL") || clean == "KEEPER":
		return "GK"
	case clean == "DEF" || strings.Contains(clean, "DEF") || clean == "CB" || clean == "FB" || clean == "LB" || clean == "RB":
		return "DEF"
	case clean == "MID" || clean == "MF" || strings.Contains(clean, "MID") || clean == "CM" || clean == "CAM" || clean == "CDM":
		return "MID"
	case clean == "ST" || clean == "FWD" || clean == "WG" || strings.Contains(clean, "STRIK") || strings.Contains(clean, "FOR") || strings.Contains(clean, "ATT") || strings.Contains(clean, "WING"):
		return "ST"
	default:
		return clean
	}
}

var _ = http.MethodGet
