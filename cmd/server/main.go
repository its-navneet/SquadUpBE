package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"squadup/backend/internal/api"
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
	"squadup/backend/internal/ws"

	"github.com/google/uuid"
)

func main() {
	cfg := config.Load()
	db := database.Open(cfg.DatabaseURL)
	if e := database.Migrate(db); e != nil {
		log.Fatal(e)
	}
	database.Seed(db)

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

	srv := api.NewServer(
		&cfg,
		db,
		as,
		gs,
		ms,
		rs,
		pushService,
		imageClient,
		imgStorage,
		hub,
		presence,
		apiLimiter,
		authLimiter,
	)

	r := srv.SetupRouter()

	log.Printf("SquadUp listening on :%s", cfg.Port)
	// Bind explicitly to IPv4 so physical devices can reach the server through
	// the Mac's LAN address (for example, 192.168.x.x).
	log.Fatal(http.ListenAndServe("0.0.0.0:"+cfg.Port, r))
}
