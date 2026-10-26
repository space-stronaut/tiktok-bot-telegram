package main

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStoreBannerUpload(t *testing.T) {
	if os.Getenv("BANNER_IT") == "" {
		t.Skip("integration; set BANNER_IT=1")
	}
	cfg := Config{
		BannerEndpoint:   os.Getenv("BANNER_S3_ENDPOINT"),
		BannerAccessKey:  os.Getenv("BANNER_S3_ACCESS_KEY"),
		BannerSecretKey:  os.Getenv("BANNER_S3_SECRET"),
		BannerBucket:     os.Getenv("BANNER_S3_BUCKET"),
		BannerPublicBase: strings.TrimRight(os.Getenv("BANNER_PUBLIC_BASE"), "/"),
	}
	if cfg.BannerEndpoint == "" || cfg.BannerAccessKey == "" || cfg.BannerSecretKey == "" || cfg.BannerBucket == "" || cfg.BannerPublicBase == "" {
		t.Skip("config tidak lengkap; set BANNER_S3_* dan BANNER_PUBLIC_BASE")
	}
	png := bytes.Repeat([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00}, 240)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("home_banner_file", "b.png")
	_, _ = fw.Write(png)
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/settings", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req = req.WithContext(context.Background())
	app := &App{cfg: cfg, loc: time.UTC}
	s := &AdminServer{app: app, loc: time.UTC}
	url, err := s.storeBannerUpload(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("public fetch: %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
}
