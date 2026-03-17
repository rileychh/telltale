package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rileychh/telltale/internal/github"
	"github.com/rileychh/telltale/internal/store"
	"github.com/rileychh/telltale/internal/telegram"
)

func main() {
	cfg := loadConfig()

	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	tg, err := telegram.New(cfg.TelegramToken, cfg.TelegramChatID)
	if err != nil {
		log.Fatalf("failed to create telegram bot: %v", err)
	}

	ghClient, err := github.NewClient(cfg.GitHubAppID, cfg.GitHubPrivateKey)
	if err != nil {
		log.Fatalf("failed to create github client: %v", err)
	}

	ghHandler := github.NewHandler(cfg.GitHubWebhookSecret, tg, db)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/github", ghHandler.ServeHTTP)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tg.RegisterReplyHandler(mux, "/webhook/telegram", db, ghClient)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go tg.StartWebhook(ctx)

	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
}

type config struct {
	TelegramToken       string
	TelegramChatID      string
	GitHubWebhookSecret string
	GitHubAppID         int64
	GitHubPrivateKey    []byte
	DatabasePath        string
	Port                string
}

func loadConfig() config {
	appID, _ := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)

	var privateKey []byte
	if path := os.Getenv("GITHUB_PRIVATE_KEY_PATH"); path != "" {
		var err error
		privateKey, err = os.ReadFile(path)
		if err != nil {
			log.Fatalf("failed to read private key: %v", err)
		}
	}

	cfg := config{
		TelegramToken:       os.Getenv("TELEGRAM_TOKEN"),
		TelegramChatID:      os.Getenv("TELEGRAM_CHAT_ID"),
		GitHubWebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
		GitHubAppID:         appID,
		GitHubPrivateKey:    privateKey,
		DatabasePath:        os.Getenv("DATABASE_PATH"),
		Port:                os.Getenv("PORT"),
	}

	if cfg.TelegramToken == "" {
		log.Fatal("TELEGRAM_TOKEN is required")
	}
	if cfg.TelegramChatID == "" {
		log.Fatal("TELEGRAM_CHAT_ID is required")
	}
	if cfg.GitHubWebhookSecret == "" {
		log.Fatal("GITHUB_WEBHOOK_SECRET is required")
	}
	if cfg.GitHubAppID == 0 {
		log.Fatal("GITHUB_APP_ID is required")
	}
	if cfg.GitHubPrivateKey == nil {
		log.Fatal("GITHUB_PRIVATE_KEY_PATH is required")
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "telltale.db"
	}
	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	return cfg
}
