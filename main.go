package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Config struct {
	BotToken          string
	AdminIDs          []int64
	S3Endpoint        string
	S3AccessKey       string
	S3SecretKey       string
	S3Bucket          string
	S3Region          string
	BannerEndpoint    string
	BannerAccessKey   string
	BannerSecretKey   string
	BannerBucket      string
	BannerPublicBase  string
	MySQLDSN          string
	MaxAccounts       int
	MaxZipBytes       int64
	PricePerAccount   int64
	PanzzBaseURL      string
	PanzzAPIKey       string
	PaymentWebhookURL string
	AdminContactURL   string
	TZ                string
	AdminAddr         string
	HomeBannerURL     string
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	cfg := Config{
		BotToken:          getenv("BOT_TOKEN", ""),
		S3Endpoint:        getenv("S3_ENDPOINT", "http://127.0.0.1:9001"),
		S3AccessKey:       getenv("S3_ACCESS_KEY", ""),
		S3SecretKey:       getenv("S3_SECRET_KEY", ""),
		S3Bucket:          getenv("S3_BUCKET", "tiktok-account"),
		S3Region:          getenv("S3_REGION", "us-east-1"),
		BannerEndpoint:    getenv("BANNER_S3_ENDPOINT", "http://127.0.0.1:9000"),
		BannerAccessKey:   getenv("BANNER_S3_ACCESS_KEY", ""),
		BannerSecretKey:   getenv("BANNER_S3_SECRET", ""),
		BannerBucket:      getenv("BANNER_S3_BUCKET", "tiktok-banner"),
		BannerPublicBase:  strings.TrimRight(getenv("BANNER_PUBLIC_BASE", ""), "/"),
		MySQLDSN:          getenv("MYSQL_DSN", ""),
		MaxAccounts:       100,
		MaxZipBytes:       45000000,
		PricePerAccount:   1000,
		PanzzBaseURL:      strings.TrimRight(getenv("PANZZ_BASE_URL", "https://panzzpay.my.id"), "/"),
		PanzzAPIKey:       getenv("PANZZ_API_KEY", ""),
		PaymentWebhookURL: getenv("PAYMENT_WEBHOOK_URL", "https://tiktokbotmanager.naldstronaut.my.id/webhook/payment"),
		AdminContactURL:   getenv("ADMIN_CONTACT_URL", ""),
		TZ:                getenv("TZ", "Asia/Jakarta"),
		AdminAddr:         getenv("ADMIN_ADDR", ":8091"),
		HomeBannerURL:     getenv("HOME_BANNER_URL", ""),
	}

	if v := getenv("MAX_ACCOUNTS_PER_ORDER", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxAccounts = n
		}
	}
	if v := getenv("MAX_ZIP_BYTES", ""); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MaxZipBytes = n
		}
	}
	if v := getenv("PRICE_PER_ACCOUNT", ""); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.PricePerAccount = n
		}
	}
	for _, part := range strings.Split(getenv("BOT_ADMIN_IDS", ""), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if id, err := strconv.ParseInt(part, 10, 64); err == nil {
			cfg.AdminIDs = append(cfg.AdminIDs, id)
		}
	}

	if cfg.BotToken == "" {
		log.Fatal("BOT_TOKEN wajib diisi")
	}
	if cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		log.Fatal("S3_ACCESS_KEY dan S3_SECRET_KEY wajib diisi")
	}
	if cfg.MySQLDSN == "" {
		log.Fatal("MYSQL_DSN wajib diisi")
	}
	if cfg.PanzzAPIKey == "" {
		log.Fatal("PANZZ_API_KEY wajib diisi")
	}
	return cfg
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	cfg := loadConfig()

	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		log.Fatalf("timezone %q tidak valid: %v", cfg.TZ, err)
	}

	store, err := NewStore(cfg, loc)
	if err != nil {
		log.Fatalf("inisialisasi store: %v", err)
	}
	defer store.Close()

	settings, err := store.LoadSettings(context.Background())
	if err != nil {
		log.Printf("muat settings: %v", err)
	} else {
		applySettings(&cfg, settings)
	}

	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Fatalf("koneksi telegram: %v", err)
	}
	api.Client = &http.Client{Timeout: 15 * time.Minute}
	log.Printf("bot @%s siap", api.Self.UserName)

	pay := NewPanzzPay(cfg.PanzzBaseURL, cfg.PanzzAPIKey, cfg.PaymentWebhookURL)
	app := newApp(cfg, store, pay, api, loc)
	app.loadTexts(settings)
	app.setupBotMenu()
	go app.startDeliveryWorker()
	ctx := context.Background()

	admin, err := NewAdminServer(app)
	if err != nil {
		log.Fatalf("inisialisasi panel admin: %v", err)
	}
	if err := admin.SeedAdmin(ctx); err != nil {
		log.Fatalf("seed panel admin: %v", err)
	}
	go func() {
		if err := admin.Start(ctx, cfg.AdminAddr); err != nil {
			log.Printf("panel admin berhenti: %v", err)
		}
	}()

	if err := store.ReleaseStale(ctx); err != nil {
		log.Printf("release stale: %v", err)
	}
	if res, err := store.SyncReconcile(ctx); err != nil {
		log.Printf("sync awal gagal: %v", err)
	} else {
		log.Printf("sync awal: %d akun (remap %d, stale %d, duplikat %d)",
			res.Total, res.Remapped, res.Pruned, res.Duplicates)
	}

	go app.maintenance(ctx)
	go app.resumePayments(ctx)
	go app.run()
	log.Printf("panel admin listening di %s", cfg.AdminAddr)
	select {}
}
