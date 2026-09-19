package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
	"squadup/backend/internal/redisx"
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

	rClient := redisx.New(&cfg)
	defer rClient.Close()

	locker := redisx.NewLocker(rClient)
	cache := redisx.NewCache(rClient)
	queue := redisx.NewQueue(rClient)
	queue.Start(context.Background(), 2)
	defer queue.Stop()

	go func() {
		var groupIDs []uuid.UUID
		db.Model(&models.Match{}).Where("finalized_at IS NOT NULL").Distinct("group_id").Pluck("group_id", &groupIDs)
		for _, gid := range groupIDs {
			_ = match.RecalculateGroupStats(db, gid)
		}
	}()

	as := auth.New(cfg.JWTSecret)
	hub := ws.NewWithRedis(rClient)
	presence := ws.NewPresenceTrackerWithRedis(rClient)
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
		cfg.GeminiAPIKey,
		cfg.GeminiImageModel,
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
		apiLimiter = limiter.NewDistributed(cfg.RateLimitRPS, cfg.RateLimitBurst, 5*time.Minute, 15*time.Minute, rClient)
		defer apiLimiter.Stop()

		authRatePerSec := float64(cfg.AuthRateLimitRPM) / 60.0
		authLimiter = limiter.NewDistributed(authRatePerSec, cfg.AuthRateLimitBurst, 5*time.Minute, 15*time.Minute, rClient)
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
	srv.SetRedis(rClient, locker, cache, queue)

	r := srv.SetupRouter()

	httpServer := &http.Server{
		Addr:              "0.0.0.0:" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	// Channel to listen for interrupt signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Printf("SquadUp listening on :%s", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-sigChan
	log.Println("[Server] Graceful shutdown initiated...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[Server] Forced server shutdown: %v", err)
	}

	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}

	log.Println("[Server] SquadUp server stopped cleanly")
}
