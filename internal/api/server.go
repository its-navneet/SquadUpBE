package api

import (
	"squadup/backend/internal/auth"
	"squadup/backend/internal/client"
	"squadup/backend/internal/config"
	"squadup/backend/internal/group"
	"squadup/backend/internal/limiter"
	"squadup/backend/internal/match"
	"squadup/backend/internal/push"
	"squadup/backend/internal/rating"
	"squadup/backend/internal/redisx"
	"squadup/backend/internal/storage"
	"squadup/backend/internal/ws"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Server struct {
	cfg           *config.Config
	db            *gorm.DB
	authService   *auth.Service
	groupService  *group.Service
	matchService  *match.Service
	ratingService *rating.Service
	pushService   *push.Service
	imageClient   *client.ImageClient
	storage       storage.Storage
	hub           *ws.Hub
	presence      *ws.PresenceTracker
	apiLimiter    *limiter.Limiter
	authLimiter   *limiter.Limiter
	redisClient   *redisx.Client
	locker        *redisx.Locker
	cache         *redisx.Cache
	queue         *redisx.Queue
}

func NewServer(
	cfg *config.Config,
	db *gorm.DB,
	authService *auth.Service,
	groupService *group.Service,
	matchService *match.Service,
	ratingService *rating.Service,
	pushService *push.Service,
	imageClient *client.ImageClient,
	storage storage.Storage,
	hub *ws.Hub,
	presence *ws.PresenceTracker,
	apiLimiter *limiter.Limiter,
	authLimiter *limiter.Limiter,
) *Server {
	return &Server{
		cfg:           cfg,
		db:            db,
		authService:   authService,
		groupService:  groupService,
		matchService:  matchService,
		ratingService: ratingService,
		pushService:   pushService,
		imageClient:   imageClient,
		storage:       storage,
		hub:           hub,
		presence:      presence,
		apiLimiter:    apiLimiter,
		authLimiter:   authLimiter,
		locker:        redisx.NewLocker(nil),
		cache:         redisx.NewCache(nil),
		queue:         redisx.NewQueue(nil),
	}
}

// SetRedis configures the Redis client and components on the Server.
func (s *Server) SetRedis(rc *redisx.Client, locker *redisx.Locker, cache *redisx.Cache, queue *redisx.Queue) {
	s.redisClient = rc
	if locker != nil {
		s.locker = locker
	}
	if cache != nil {
		s.cache = cache
	}
	if queue != nil {
		s.queue = queue
	}
}

func cors(origin string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Request-ID")
		c.Header("Access-Control-Expose-Headers", "X-Request-ID")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		if c.Request.Method == "OPTIONS" {
			c.Status(204)
			return
		}
		c.Next()
	}
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Permissions-Policy", "geolocation=(), camera=(), microphone=()")
		c.Next()
	}
}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		c.Header("X-Request-ID", rid)
		c.Set("requestID", rid)
		c.Next()
	}
}

func (s *Server) SetupRouter() *gin.Engine {
	r := gin.Default()
	_ = r.SetTrustedProxies(nil)
	r.Use(requestID())
	r.Use(securityHeaders())
	r.Use(cors(s.cfg.CORSOrigins))
	r.GET("/health", func(c *gin.Context) {
		redisStatus := "in-memory-fallback"
		if s.redisClient != nil && s.redisClient.IsAvailable() {
			redisStatus = "connected"
		}
		c.JSON(200, gin.H{
			"status": "ok",
			"redis":  redisStatus,
		})
	})
	r.Static("/uploads", "./uploads")

	apiGroup := r.Group("/api")
	if s.apiLimiter != nil {
		apiGroup.Use(s.apiLimiter.Middleware(limiter.UserOrIPKeyExtractor))
	}

	var authLimitMiddleware gin.HandlerFunc
	if s.authLimiter != nil {
		authLimitMiddleware = s.authLimiter.Middleware(limiter.IPKeyExtractor)
	} else {
		authLimitMiddleware = func(c *gin.Context) { c.Next() }
	}

	// Public routes
	apiGroup.GET("/matches/:id/poster-image", s.ServePosterImage)
	apiGroup.POST("/upload/image", authLimitMiddleware, s.UploadImage)
	apiGroup.POST("/auth/register", authLimitMiddleware, s.Register)
	apiGroup.POST("/auth/login", authLimitMiddleware, s.Login)
	apiGroup.POST("/auth/reset-password", authLimitMiddleware, s.ResetPassword)

	// Authenticated routes
	sec := apiGroup.Group("")
	sec.Use(s.authService.Middleware())

	// Users
	sec.GET("/users/me", s.GetMe)
	sec.GET("/users/:id", s.GetUser)
	sec.PUT("/users/me", s.UpdateMe)
	sec.PUT("/users/me/password", s.ChangePassword)
	sec.POST("/users/device-token", s.RegisterDeviceToken)
	sec.DELETE("/users/device-token", s.DeleteDeviceToken)

	// Sports
	sec.GET("/sports", s.ListSports)

	// Groups
	sec.POST("/groups", s.CreateGroup)
	sec.GET("/groups", s.ListGroups)
	sec.GET("/groups/:id", s.GetGroup)
	sec.PATCH("/groups/:id", s.UpdateGroup)
	sec.DELETE("/groups/:id", s.DeleteGroup)
	sec.GET("/groups/:id/members", s.ListMembers)
	sec.PUT("/groups/:id/members/:userId/role", s.UpdateMemberRole)
	sec.POST("/groups/:id/members/:userId/role", s.UpdateMemberRole)
	sec.DELETE("/groups/:id/members/:userId", s.RemoveMember)
	sec.GET("/groups/:id/join-requests", s.ListJoinRequests)
	sec.POST("/groups/join", s.JoinGroup)
	sec.GET("/groups/join-requests/my", s.GetMyJoinRequests)
	sec.GET("/groups/join-requests/:requestId/status", s.GetJoinRequestStatus)
	sec.DELETE("/groups/join-requests/:requestId", s.CancelJoinRequest)
	sec.POST("/groups/:id/join-requests/:requestId/approve", s.ApproveJoinRequest)
	sec.POST("/groups/:id/join-requests/:requestId/reject", s.RejectJoinRequest)

	// Ratings & Leaderboard
	sec.GET("/groups/:id/ratings/:userId", s.GetUserRating)
	sec.POST("/groups/:id/ratings", s.UpsertUserRating)

	// Notifications
	sec.GET("/notifications", s.ListNotifications)
	sec.GET("/notifications/unread-count", s.GetUnreadNotificationCount)
	sec.POST("/notifications/:id/read", s.MarkNotificationRead)
	sec.POST("/notifications/read-all", s.MarkAllNotificationsRead)
	sec.DELETE("/notifications/:id", s.DeleteNotification)
	sec.DELETE("/notifications/clear-all", s.ClearAllNotifications)

	// Chat
	sec.GET("/groups/:id/chat", s.GetChatMessages)
	sec.POST("/groups/:id/chat/read", s.MarkChatRead)
	sec.GET("/groups/:id/chat/unread", s.GetUnreadChatCount)
	sec.POST("/groups/:id/chat/typing", s.SendTypingStatus)
	sec.GET("/groups/:id/chat/messages/:message_id/seen", s.GetSeenStatus)
	sec.GET("/groups/:id/chat/:message_id/seen", s.GetSeenStatus)
	sec.POST("/groups/:id/chat", s.SendChatMessage)
	sec.DELETE("/groups/:id/chat/:messageId", s.DeleteChatMessage)

	// WebSockets & Presence
	sec.GET("/presence/online", s.GetOnlinePresence)
	sec.GET("/ws/presence", s.PresenceWS)
	sec.GET("/ws/groups/:id", s.GroupWS)
	sec.GET("/ws/matches/:id", s.MatchWS)

	// Venues
	sec.POST("/groups/:id/venues", s.CreateVenue)
	sec.GET("/groups/:id/venues", s.ListVenues)
	sec.PUT("/groups/:id/venues/:venueId", s.UpdateVenue)
	sec.DELETE("/groups/:id/venues/:venueId", s.DeleteVenue)

	// Group Matches
	sec.POST("/groups/:id/matches", s.CreateMatch)
	sec.GET("/groups/:id/matches", s.ListGroupMatches)

	// Match Polls
	sec.POST("/groups/:id/polls", s.CreatePoll)
	sec.GET("/groups/:id/polls/active", s.GetActivePoll)
	sec.POST("/groups/:id/polls/:pollId/vote", s.VotePoll)
	sec.DELETE("/groups/:id/polls/:pollId", s.DeletePoll)

	// Squad leaderboard
	sec.GET("/groups/:id/leaderboard", s.GetLeaderboard)

	// Match-scoped operations
	matchSec := sec.Group("", s.MatchMembershipMiddleware())
	matchSec.GET("/matches/:id", s.GetMatch)
	matchSec.PUT("/matches/:id", s.UpdateMatch)
	matchSec.DELETE("/matches/:id", s.DeleteMatch)
	matchSec.POST("/matches/:id/attendance", s.MarkAttendance)
	matchSec.GET("/matches/:id/attendance", s.GetAttendance)
	matchSec.POST("/matches/:id/generate-teams", s.GenerateTeams)
	matchSec.GET("/matches/:id/teams", s.GetMatchTeams)
	matchSec.POST("/matches/:id/teams/move-player", s.MovePlayer)
	matchSec.POST("/matches/:id/teams/swap-players", s.SwapPlayers)
	matchSec.GET("/matches/:id/mini-matches", s.GetMiniMatches)
	matchSec.POST("/matches/:id/mini-matches/rotate", s.RotateMiniMatch)
	matchSec.POST("/matches/:id/mini-matches/start", s.StartMiniMatch)
	matchSec.POST("/matches/:id/start", s.StartMatch)
	matchSec.POST("/matches/:id/finish", s.FinishMatch)
	matchSec.POST("/matches/:id/events", s.AddMatchEvent)
	matchSec.DELETE("/matches/:id/events/:eventId", s.DeleteMatchEvent)
	matchSec.GET("/matches/:id/events", s.GetMatchEvents)
	matchSec.GET("/matches/:id/result", s.GetMatchResult)
	matchSec.POST("/matches/:id/finalize", s.FinalizeMatch)

	return r
}
