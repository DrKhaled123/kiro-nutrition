package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nutrition-health-backend/internal/config"
	"nutrition-health-backend/internal/database"
	"nutrition-health-backend/internal/handlers"
	"nutrition-health-backend/internal/middleware"
	"nutrition-health-backend/internal/redis"
	"nutrition-health-backend/internal/services"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ No .env file found, using system environment")
	}

	// Check for command-line flags
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-migrate", "--migrate":
			runMigrations()
			return
		case "-seed", "--seed":
			runSeeding()
			return
		case "-reset", "--reset":
			runReset()
			return
		}
	}

	// Load configuration
	cfg := config.Load()
	log.Printf("🚀 Starting Nutrition Health Backend v%s", cfg.API.Version)
	log.Printf("🌍 Environment: %s", cfg.Server.Environment)

	// Initialize database
	db, err := database.Initialize(cfg.Database.Path)
	if err != nil {
		log.Fatalf("❌ Database init failed: %v", err)
	}
	defer db.Close()
	log.Println("✅ Database connected")

	// Initialize Redis
	redisClient := redis.Initialize(cfg.Redis)
	if redisClient != nil {
		log.Println("✅ Redis connected")
	} else {
		log.Println("⚠️ Redis unavailable (degraded caching)")
	}

	// Initialize services with DI
	services := services.NewServices(db, redisClient, cfg)
	log.Println("✅ Services initialized")

	// Initialize Echo
	e := echo.New()
	e.HideBanner = true

	// Setup structured logging
	middleware.SetupLogger(cfg.Server.Environment)

	// Core middleware
	e.Use(echomiddleware.Recover())
	e.Use(middleware.CorrelationID())
	e.Use(middleware.StructuredLogger())
	e.Use(middleware.ErrorLogger())
	e.Use(middleware.AuditLogger())

	// Custom middleware
	e.Use(middleware.Security())
	e.Use(middleware.CORS(cfg.Security.CORSOrigins))

	// Distributed rate limiting with Redis
	if redisClient != nil {
		rateLimitConfig := middleware.RateLimitConfig{
			Client: redisClient,
			Limit:  int64(cfg.Security.RateLimitReqs),
			Window: cfg.Security.RateLimitWindow,
		}
		e.Use(middleware.DistributedRateLimiter(rateLimitConfig))
	}

	e.Use(middleware.Compression())

	// Health check endpoints (Kubernetes-ready)
	healthCheckHandler := handlers.NewHealthCheckHandler(services)
	e.GET("/health", healthCheckHandler.Health)
	e.GET("/health/live", healthCheckHandler.Liveness)
	e.GET("/health/ready", healthCheckHandler.Readiness)
	e.GET("/health/startup", healthCheckHandler.Startup)

	e.GET("/disclaimer", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"disclaimer":    "This information is for educational purposes only and does not replace professional medical advice. Please consult with a healthcare provider before making any dietary or health changes.",
			"disclaimer_ar": "هذه المعلومات لأغراض تعليمية فقط ولا تحل محل الاستشارة الطبية المهنية. يرجى استشارة مقدم الرعاية الصحية قبل إجراء أي تغييرات غذائية أو صحية.",
		})
	})

	// API routes
	api := e.Group("/api/" + cfg.API.Version)
	handlers.RegisterRoutes(api, services, cfg)
	log.Println("✅ Routes registered")

	// Start server
	go func() {
		addr := ":" + cfg.Server.Port
		log.Printf("🌐 Server starting on http://localhost%s", addr)
		log.Printf("📊 Health: http://localhost%s/health", addr)
		log.Printf("📖 API: http://localhost%s/api/%s", addr, cfg.API.Version)

		if err := e.Start(addr); err != nil && err != http.ErrServerClosed {
			log.Fatalf("❌ Server failed: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	log.Println("🛑 Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Cleanup services
	if err := services.Cleanup(); err != nil {
		log.Printf("⚠️ Service cleanup error: %v", err)
	}

	if err := e.Shutdown(ctx); err != nil {
		log.Fatalf("❌ Forced shutdown: %v", err)
	}

	log.Println("✅ Server stopped gracefully")
}

// runMigrations runs database migrations
func runMigrations() {
	log.Println("🔄 Running database migrations...")

	cfg := config.Load()
	db, err := database.Initialize(cfg.Database.Path)
	if err != nil {
		log.Fatalf("❌ Database init failed: %v", err)
	}
	defer db.Close()

	if err := database.RunMigrations(db); err != nil {
		log.Fatalf("❌ Migration failed: %v", err)
	}

	if err := database.VerifySchema(db); err != nil {
		log.Fatalf("❌ Schema verification failed: %v", err)
	}

	log.Println("✅ Migrations completed successfully")
}

// runSeeding seeds the database with initial data
func runSeeding() {
	log.Println("🌱 Seeding database...")

	cfg := config.Load()
	db, err := database.Initialize(cfg.Database.Path)
	if err != nil {
		log.Fatalf("❌ Database init failed: %v", err)
	}
	defer db.Close()

	seeder := database.NewSeeder(db)
	if err := seeder.SeedAll(); err != nil {
		log.Fatalf("❌ Seeding failed: %v", err)
	}

	log.Println("✅ Seeding completed successfully")
}

// runReset resets the database (drops and recreates)
func runReset() {
	log.Println("🔄 Resetting database...")

	cfg := config.Load()

	// Remove existing database
	if err := os.Remove(cfg.Database.Path); err != nil && !os.IsNotExist(err) {
		log.Fatalf("❌ Failed to remove database: %v", err)
	}

	// Run migrations
	runMigrations()

	// Run seeding
	runSeeding()

	log.Println("✅ Database reset completed successfully")
}
