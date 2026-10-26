package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

//go:embed templates/*.html templates/app.js
var adminAssets embed.FS

const (
	adminCookieName = "tbm_session"
	adminSessionTTL = 12 * time.Hour
	adminPageSize   = 25
)

type AdminServer struct {
	app       *App
	store     *Store
	loc       *time.Location
	pages     map[string]*template.Template
	loginTmpl *template.Template
	limiter   *loginLimiter
}

type sessionCtxKey struct{}

type pageData struct {
	Title         string
	Active        string
	User          string
	BotName       string
	Flash         string
	FlashKind     string
	CSRF          string
	Err           string
	Stats         *AdminStats
	Orders        []Order
	Order         *Order
	Accounts      []Account
	Stock         []AdminStockRow
	Settings      *settingsView
	Texts         []textView
	Query         string
	StatusFilter  string
	InvoiceFilter string
	Page          int
	TotalPages    int
	Total         int
	PrevURL       string
	NextURL       string
	DefaultPrice  int64
	MaxAccounts   int
}

type settingsView struct {
	BotTokenMasked  string
	BotTokenSet     bool
	AdminIDs        string
	PricePerAccount int64
	MaxAccounts     int
	MaxZipMB        int64
	BotName         string
	HomeBannerURL   string
	BannerUpload    bool
}

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginEntry
}

type loginEntry struct {
	count int
	reset time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]*loginEntry)}
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok || time.Now().After(e.reset) {
		return true
	}
	return e.count < 8
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok || time.Now().After(e.reset) {
		l.entries[ip] = &loginEntry{count: 1, reset: time.Now().Add(10 * time.Minute)}
		return
	}
	e.count++
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

func NewAdminServer(app *App) (*AdminServer, error) {
	loc := app.loc
	funcs := template.FuncMap{
		"rupiah": rupiah,
		"date": func(t time.Time) string {
			return formatDateID(t.In(loc))
		},
		"datetime": func(t time.Time) string {
			return t.In(loc).Format("02 Jan 2006 15:04")
		},
		"mb": func(n int64) string {
			return fmt.Sprintf("%.2f MB", float64(n)/1e6)
		},
		"statusClass": func(s string) string {
			switch s {
			case "pending":
				return "warning"
			case "paid":
				return "primary"
			case "processing":
				return "info"
			case "sent":
				return "success"
			case "cancelled":
				return "secondary"
			default:
				return "light"
			}
		},
		"statusText": func(s string) string {
			switch s {
			case "pending":
				return "Menunggu bayar"
			case "paid":
				return "Lunas"
			case "processing":
				return "Diproses"
			case "sent":
				return "Terkirim"
			case "cancelled":
				return "Dibatalkan"
			default:
				return s
			}
		},
		"invoiceClass": func(s string) string {
			switch s {
			case "paid":
				return "success"
			case "review":
				return "danger"
			case "expired":
				return "secondary"
			default:
				return "warning"
			}
		},
		"invoiceText": func(s string) string {
			switch s {
			case "paid":
				return "Lunas"
			case "review":
				return "Review"
			case "expired":
				return "Kedaluwarsa"
			default:
				return "Menunggu"
			}
		},
		"mask": maskSecret,
		"add":  func(a, b int) int { return a + b },
		"sub":  func(a, b int) int { return a - b },
		"isManual": func(id string) bool {
			return strings.HasPrefix(id, "MAN-")
		},
		"divpct": func(part, total int) int {
			if total <= 0 {
				return 0
			}
			return part * 100 / total
		},
		"q": func(v any) string {
			return url.QueryEscape(fmt.Sprint(v))
		},
	}

	pages := make(map[string]*template.Template)
	for _, name := range []string{"dashboard", "orders", "order", "stock", "settings", "texts", "password", "neworder"} {
		t, err := template.New("layout").Funcs(funcs).ParseFS(adminAssets, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		pages[name] = t
	}
	loginTmpl, err := template.New("login").Funcs(funcs).ParseFS(adminAssets, "templates/login.html")
	if err != nil {
		return nil, fmt.Errorf("template login: %w", err)
	}

	return &AdminServer{
		app:       app,
		store:     app.store,
		loc:       loc,
		pages:     pages,
		loginTmpl: loginTmpl,
		limiter:   newLoginLimiter(),
	}, nil
}

func (s *AdminServer) SeedAdmin(ctx context.Context) error {
	cfg := s.app.cfg
	defaults := map[string]string{
		settingsKeyAdminIDs:    joinIDs(cfg.AdminIDs),
		settingsKeyPrice:       strconv.FormatInt(cfg.PricePerAccount, 10),
		settingsKeyMaxAccounts: strconv.Itoa(cfg.MaxAccounts),
		settingsKeyMaxZipBytes: strconv.FormatInt(cfg.MaxZipBytes, 10),
	}
	if err := s.store.SeedSettings(ctx, defaults); err != nil {
		return err
	}

	n, err := s.store.CountAdminUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	username := strings.TrimSpace(getenv("ADMIN_USER", "admin"))
	if username == "" {
		username = "admin"
	}
	password := getenv("ADMIN_PASS", "")
	generated := false
	if password == "" {
		password = randomHex(9)
		generated = true
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	if err := s.store.CreateAdminUser(ctx, username, hash); err != nil {
		return err
	}
	if generated {
		log.Printf("panel admin: user %q dibuat, password sementara %q (segera ganti)", username, password)
	} else {
		log.Printf("panel admin: user %q dibuat", username)
	}
	return nil
}

func (s *AdminServer) Start(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("panel admin listening di %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *AdminServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /static/app.js", s.handleAppJS)
	mux.HandleFunc("GET /login", s.handleLoginGet)
	mux.HandleFunc("POST /login", s.handleLoginPost)
	mux.HandleFunc("POST /logout", s.withAuthPost(s.handleLogout))
	mux.HandleFunc("GET /", s.withAuth(s.handleDashboard))
	mux.HandleFunc("GET /orders", s.withAuth(s.handleOrders))
	mux.HandleFunc("GET /orders/new", s.withAuth(s.handleNewOrderGet))
	mux.HandleFunc("POST /orders/new", s.withAuthPost(s.handleNewOrderPost))
	mux.HandleFunc("GET /orders/{id}", s.withAuth(s.handleOrderDetail))
	mux.HandleFunc("GET /orders/{id}/download", s.withAuth(s.handleOrderDownload))
	mux.HandleFunc("POST /orders/{id}/sync", s.withAuthPost(s.handleOrderSync))
	mux.HandleFunc("POST /orders/{id}/mark-paid", s.withAuthPost(s.handleOrderMarkPaid))
	mux.HandleFunc("POST /orders/{id}/deliver", s.withAuthPost(s.handleOrderDeliver))
	mux.HandleFunc("POST /orders/{id}/cancel", s.withAuthPost(s.handleOrderCancel))
	mux.HandleFunc("GET /stock", s.withAuth(s.handleStock))
	mux.HandleFunc("GET /settings", s.withAuth(s.handleSettingsGet))
	mux.HandleFunc("POST /settings", s.withAuthPost(s.handleSettingsPost))
	mux.HandleFunc("GET /texts", s.withAuth(s.handleTextsGet))
	mux.HandleFunc("POST /texts", s.withAuthPost(s.handleTextsPost))
	mux.HandleFunc("GET /password", s.withAuth(s.handlePasswordGet))
	mux.HandleFunc("POST /password", s.withAuthPost(s.handlePasswordPost))
	return securityHeaders(mux)
}

func (s *AdminServer) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.currentSession(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, sess)))
	}
}

func (s *AdminServer) withAuthPost(next http.HandlerFunc) http.HandlerFunc {
	return s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := r.Context().Value(sessionCtxKey{}).(*AdminSession)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
			if err := r.ParseMultipartForm(8 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
				http.Error(w, "form tidak valid: "+err.Error(), http.StatusBadRequest)
				return
			}
		} else if err := r.ParseForm(); err != nil {
			http.Error(w, "form tidak valid", http.StatusBadRequest)
			return
		}
		if sess == nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
			http.Error(w, "CSRF token tidak valid", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func (s *AdminServer) currentSession(r *http.Request) (*AdminSession, error) {
	c, err := r.Cookie(adminCookieName)
	if err != nil {
		return nil, err
	}
	sess, err := s.store.GetAdminSession(r.Context(), c.Value)
	if err != nil {
		return nil, err
	}
	if !sess.ExpiresAt.After(time.Now()) {
		_ = s.store.DeleteAdminSession(r.Context(), sess.Token)
		return nil, sql.ErrNoRows
	}
	if time.Until(sess.ExpiresAt) < adminSessionTTL/2 {
		_ = s.store.TouchAdminSession(r.Context(), sess.Token, time.Now().Add(adminSessionTTL))
	}
	return sess, nil
}

func (s *AdminServer) handleAppJS(w http.ResponseWriter, r *http.Request) {
	js, err := adminAssets.ReadFile("templates/app.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(js)
}

func (s *AdminServer) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if _, err := s.currentSession(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, http.StatusOK, "")
}

func (s *AdminServer) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form tidak valid", http.StatusBadRequest)
		return
	}
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		s.renderLogin(w, http.StatusTooManyRequests, "Terlalu banyak percobaan. Coba lagi beberapa menit.")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	user, err := s.store.AdminUserByUsername(r.Context(), username)
	if err != nil || !verifyPassword(user.PasswordHash, password) {
		s.limiter.fail(ip)
		s.renderLogin(w, http.StatusUnauthorized, "Username atau password salah.")
		return
	}
	s.limiter.reset(ip)

	token := randomHex(32)
	csrf := randomHex(16)
	expires := time.Now().Add(adminSessionTTL)
	if err := s.store.CreateAdminSession(r.Context(), token, csrf, user.ID, expires); err != nil {
		log.Printf("panel admin: buat session: %v", err)
		http.Error(w, "gagal membuat session", http.StatusInternalServerError)
		return
	}
	_ = s.store.DeleteExpiredAdminSessions(r.Context())
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		Expires:  expires,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *AdminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess, _ := r.Context().Value(sessionCtxKey{}).(*AdminSession)
	if sess != nil {
		_ = s.store.DeleteAdminSession(r.Context(), sess.Token)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *AdminServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.store.AdminStats(ctx)
	if err != nil {
		s.renderError(w, r, "Gagal memuat statistik: "+err.Error())
		return
	}
	orders, _, err := s.store.AdminOrders(ctx, "", "", "", 10, 0)
	if err != nil {
		s.renderError(w, r, "Gagal memuat pesanan: "+err.Error())
		return
	}
	stock, err := s.store.AdminStock(ctx)
	if err != nil {
		s.renderError(w, r, "Gagal memuat stok: "+err.Error())
		return
	}
	s.render(w, r, "dashboard", &pageData{
		Title:  "Dashboard",
		Active: "dashboard",
		Stats:  stats,
		Orders: orders,
		Stock:  stock,
	})
}

func (s *AdminServer) handleOrders(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	invoice := strings.TrimSpace(r.URL.Query().Get("invoice"))
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}

	orders, total, err := s.store.AdminOrders(r.Context(), q, status, invoice, adminPageSize, (page-1)*adminPageSize)
	if err != nil {
		s.renderError(w, r, "Gagal memuat pesanan: "+err.Error())
		return
	}
	totalPages := (total + adminPageSize - 1) / adminPageSize
	if totalPages < 1 {
		totalPages = 1
	}

	s.render(w, r, "orders", &pageData{
		Title:         "Pesanan",
		Active:        "orders",
		Orders:        orders,
		Query:         q,
		StatusFilter:  status,
		InvoiceFilter: invoice,
		Page:          page,
		TotalPages:    totalPages,
		Total:         total,
		PrevURL:       ordersURL(q, status, invoice, page-1),
		NextURL:       ordersURL(q, status, invoice, page+1),
	})
}

func (s *AdminServer) handleNewOrderGet(w http.ResponseWriter, r *http.Request) {
	stock, err := s.store.AdminStock(r.Context())
	if err != nil {
		s.renderError(w, r, "Gagal memuat stok: "+err.Error())
		return
	}
	s.render(w, r, "neworder", &pageData{
		Title:        "Order Manual",
		Active:       "orders",
		Stock:        stock,
		DefaultPrice: s.app.cfg.PricePerAccount,
		MaxAccounts:  s.app.cfg.MaxAccounts,
	})
}

func (s *AdminServer) handleNewOrderPost(w http.ResponseWriter, r *http.Request) {
	date := strings.TrimSpace(r.PostFormValue("account_date"))
	qty := atoiDefault(r.PostFormValue("quantity"), 0)
	username := strings.TrimSpace(r.PostFormValue("username"))
	price := atoi64Default(r.PostFormValue("price_per_account"), -1)
	chatID := int64(0)

	if v := strings.TrimSpace(r.PostFormValue("chat_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			redirectFlash(w, r, "/orders/new", "err", "Chat ID tidak valid.")
			return
		}
		chatID = n
	}
	if _, err := time.ParseInLocation("2006-01-02", date, s.loc); err != nil {
		redirectFlash(w, r, "/orders/new", "err", "Tanggal akun tidak valid.")
		return
	}
	if qty < 1 || qty > s.app.cfg.MaxAccounts {
		redirectFlash(w, r, "/orders/new", "err", fmt.Sprintf("Jumlah harus 1-%d akun.", s.app.cfg.MaxAccounts))
		return
	}
	if price < 0 {
		price = s.app.cfg.PricePerAccount
	}

	ctx := r.Context()
	avail, err := s.store.Available(ctx, date)
	if err != nil {
		redirectFlash(w, r, "/orders/new", "err", "Gagal cek stok: "+err.Error())
		return
	}
	if avail < qty {
		redirectFlash(w, r, "/orders/new", "err", fmt.Sprintf("Stok tanggal %s hanya %d akun.", formatDateString(date, s.loc), avail))
		return
	}

	orderID := manualOrderID()
	idemKey := "manual:" + randomHex(16)
	_, _, _, err = s.store.Reserve(ctx, orderID, idemKey, chatID, username, date, qty, s.app.cfg.MaxZipBytes, price)
	if err != nil {
		redirectFlash(w, r, "/orders/new", "err", "Gagal membuat order: "+err.Error())
		return
	}
	changed, err := s.store.MarkPaid(ctx, orderID)
	if err != nil || !changed {
		_ = s.store.Release(ctx, orderID)
		redirectFlash(w, r, "/orders/new", "err", "Gagal menandai order lunas.")
		return
	}
	if err := s.store.Finalize(ctx, orderID); err != nil {
		_ = s.store.Release(ctx, orderID)
		redirectFlash(w, r, "/orders/new", "err", "Gagal menyelesaikan order: "+err.Error())
		return
	}

	redirectFlash(w, r, "/orders/"+url.PathEscape(orderID), "ok",
		fmt.Sprintf("Order manual %s dibuat (%d akun). Klik Download ZIP untuk mengambil file.", orderID, qty))
}

func (s *AdminServer) handleOrderDownload(w http.ResponseWriter, r *http.Request) {
	o, err := s.store.GetOrder(r.Context(), r.PathValue("id"))
	if isNoRows(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "gagal memuat order", http.StatusInternalServerError)
		return
	}
	if o.Status != "sent" {
		redirectFlash(w, r, "/orders/"+url.PathEscape(o.ID), "err", "File hanya bisa diunduh untuk order yang sudah lunas/terkirim.")
		return
	}

	accounts, err := s.store.OrderAccounts(r.Context(), o.ID)
	if err != nil || len(accounts) == 0 {
		redirectFlash(w, r, "/orders/"+url.PathEscape(o.ID), "err", "Tidak ada akun pada order ini.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "tbm-download-*")
	if err != nil {
		http.Error(w, "gagal membuat file sementara", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)

	name := fmt.Sprintf("%s-%s-%dakun.zip", buyerFilePrefix(o), o.Date.Format("2006-01-02"), len(accounts))
	zipPath := filepath.Join(dir, name)
	if err := s.app.buildZip(ctx, accounts, zipPath); err != nil {
		log.Printf("panel admin: build zip %s: %v", o.ID, err)
		http.Error(w, "gagal menyiapkan ZIP", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, zipPath)
}

func manualOrderID() string {
	return "MAN-" + time.Now().Format("20060102-150405") + "-" + randomHex(3)
}

func (s *AdminServer) handleOrderDetail(w http.ResponseWriter, r *http.Request) {
	o, err := s.store.GetOrder(r.Context(), r.PathValue("id"))
	if isNoRows(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.renderError(w, r, "Gagal memuat pesanan: "+err.Error())
		return
	}
	accounts, err := s.store.OrderAccounts(r.Context(), o.ID)
	if err != nil {
		s.renderError(w, r, "Gagal memuat akun pesanan: "+err.Error())
		return
	}
	s.render(w, r, "order", &pageData{
		Title:    "Order " + o.ID,
		Active:   "orders",
		Order:    o,
		Accounts: accounts,
	})
}

func (s *AdminServer) handleOrderSync(w http.ResponseWriter, r *http.Request) {
	o, ok := s.loadOrder(w, r)
	if !ok {
		return
	}
	detail := "/orders/" + url.PathEscape(o.ID)
	if strings.TrimSpace(o.InvoiceID) == "" {
		redirectFlash(w, r, detail, "err", "Order ini belum memiliki invoice Nexus.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	inv, err := s.app.pay.InvoiceStatus(ctx, o.InvoiceID)
	if err != nil {
		redirectFlash(w, r, detail, "err", "Gagal sync ke Nexus: "+err.Error())
		return
	}
	st := paymentStatus(inv.Status)
	if err := s.store.UpdateOrderInvoice(ctx, o.ID, inv); err != nil {
		redirectFlash(w, r, detail, "err", "Gagal menyimpan status: "+err.Error())
		return
	}

	msg := fmt.Sprintf("Status Nexus: %s.", invoiceTextID(st))
	if st == "paid" {
		changed, err := s.store.MarkPaid(ctx, o.ID)
		if err != nil {
			redirectFlash(w, r, detail, "err", "Gagal menandai lunas: "+err.Error())
			return
		}
		if changed {
			s.dispatchDelivery(o.ID, false)
			msg += " Order ditandai LUNAS dan file sedang dikirim."
		} else {
			msg += " Status order tidak berubah (mungkin sudah terkirim)."
		}
	} else if st == "review" {
		msg += " Masih review; gunakan tombol Tandai Lunas & Kirim untuk bypass."
	}
	redirectFlash(w, r, detail, "ok", msg)
}

func (s *AdminServer) handleOrderMarkPaid(w http.ResponseWriter, r *http.Request) {
	o, ok := s.loadOrder(w, r)
	if !ok {
		return
	}
	detail := "/orders/" + url.PathEscape(o.ID)
	switch o.Status {
	case "sent":
		redirectFlash(w, r, detail, "err", "Order ini sudah terkirim.")
		return
	case "cancelled":
		redirectFlash(w, r, detail, "err", "Order sudah dibatalkan; buat order baru untuk pelanggan.")
		return
	}

	changed, err := s.store.MarkPaid(r.Context(), o.ID)
	if err != nil {
		redirectFlash(w, r, detail, "err", "Gagal menandai lunas: "+err.Error())
		return
	}
	if !changed {
		redirectFlash(w, r, detail, "err", "Status order tidak bisa diubah.")
		return
	}
	s.dispatchDelivery(o.ID, false)
	redirectFlash(w, r, detail, "ok", "Order ditandai LUNAS tanpa cek Nexus. File sedang dikirim ke pelanggan.")
}

func (s *AdminServer) handleOrderDeliver(w http.ResponseWriter, r *http.Request) {
	o, ok := s.loadOrder(w, r)
	if !ok {
		return
	}
	detail := "/orders/" + url.PathEscape(o.ID)
	switch o.Status {
	case "sent":
		redirectFlash(w, r, detail, "err", "Order ini sudah terkirim sebelumnya.")
		return
	case "cancelled":
		redirectFlash(w, r, detail, "err", "Order sudah dibatalkan.")
		return
	case "processing":
		redirectFlash(w, r, detail, "err", "Order sedang diproses. Tunggu beberapa saat lalu muat ulang halaman.")
		return
	}
	if o.Status == "pending" && o.InvoiceStatus == "review" {
		redirectFlash(w, r, detail, "err", "Invoice masih review di Nexus. Gunakan Tandai Lunas & Kirim untuk bypass.")
		return
	}
	s.dispatchDelivery(o.ID, true)
	redirectFlash(w, r, detail, "ok", "Permintaan kirim file dimulai dengan verifikasi status Nexus.")
}

func (s *AdminServer) handleOrderCancel(w http.ResponseWriter, r *http.Request) {
	o, ok := s.loadOrder(w, r)
	if !ok {
		return
	}
	detail := "/orders/" + url.PathEscape(o.ID)
	if o.Status != "pending" {
		redirectFlash(w, r, detail, "err", "Hanya order berstatus pending yang boleh dibatalkan dari panel.")
		return
	}
	if err := s.store.Release(r.Context(), o.ID); err != nil {
		redirectFlash(w, r, detail, "err", "Gagal membatalkan: "+err.Error())
		return
	}
	redirectFlash(w, r, detail, "ok", "Order dibatalkan dan stok akun dilepas kembali.")
}

func (s *AdminServer) handleStock(w http.ResponseWriter, r *http.Request) {
	stock, err := s.store.AdminStock(r.Context())
	if err != nil {
		s.renderError(w, r, "Gagal memuat stok: "+err.Error())
		return
	}
	stats, err := s.store.AdminStats(r.Context())
	if err != nil {
		stats = &AdminStats{}
	}
	s.render(w, r, "stock", &pageData{
		Title:  "Stok Akun",
		Active: "stock",
		Stock:  stock,
		Stats:  stats,
	})
}

func (s *AdminServer) bannerEnabled() bool {
	c := s.app.cfg
	return c.BannerPublicBase != "" && c.BannerBucket != "" && c.BannerEndpoint != "" && c.BannerAccessKey != "" && c.BannerSecretKey != ""
}

func (s *AdminServer) newBannerClient() (*minio.Client, error) {
	c := s.app.cfg
	endpoint := strings.TrimSpace(c.BannerEndpoint)
	secure := strings.HasPrefix(strings.ToLower(endpoint), "https://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimSuffix(endpoint, "/")
	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(c.BannerAccessKey, c.BannerSecretKey, ""),
		Secure: secure,
	})
}

var bannerContentTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

const bannerMaxBytes = 5 << 20

func isBannerExt(ext string) bool {
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return true
	}
	return false
}

func (s *AdminServer) storeBannerUpload(r *http.Request) (string, error) {
	f, _, err := r.FormFile("home_banner_file")
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return "", nil
		}
		return "", fmt.Errorf("gagal membaca file upload: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, bannerMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("gagal membaca isi file: %w", err)
	}
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > bannerMaxBytes {
		return "", errors.New("ukuran file banner maksimal 5 MB")
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	ct := http.DetectContentType(sniff)
	ext, ok := bannerContentTypes[ct]
	if !ok {
		return "", errors.New("file banner harus berupa gambar (jpg/png/webp/gif)")
	}

	cfg := s.app.cfg
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cl, err := s.newBannerClient()
	if err != nil {
		return "", fmt.Errorf("gagal menyambung ke object store: %w", err)
	}
	key := fmt.Sprintf("banner-%s-%s%s", time.Now().In(s.app.loc).Format("20060102-150405"), randomHex(3), ext)
	_, err = cl.PutObject(ctx, cfg.BannerBucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType:  ct,
		CacheControl: "public, max-age=2592000",
	})
	if err != nil {
		return "", fmt.Errorf("gagal mengunggah banner: %w", err)
	}
	if strings.HasPrefix(cfg.HomeBannerURL, cfg.BannerPublicBase+"/") {
		old := strings.TrimPrefix(cfg.HomeBannerURL, cfg.BannerPublicBase+"/")
		if old != "" && old != key && !strings.ContainsAny(old, "/?#") {
			if _, serr := cl.StatObject(ctx, cfg.BannerBucket, old, minio.StatObjectOptions{}); serr == nil {
				_ = cl.RemoveObject(ctx, cfg.BannerBucket, old, minio.RemoveObjectOptions{})
			}
		}
	}
	return cfg.BannerPublicBase + "/" + key, nil
}

func (s *AdminServer) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.LoadSettings(r.Context())
	if err != nil {
		s.renderError(w, r, "Gagal memuat pengaturan: "+err.Error())
		return
	}
	cfg := s.app.cfg
	s.render(w, r, "settings", &pageData{
		Title:  "Pengaturan",
		Active: "settings",
		Settings: &settingsView{
			BotTokenMasked:  maskSecret(cfg.BotToken),
			BotTokenSet:     cfg.BotToken != "",
			AdminIDs:        strings.TrimSpace(settings[settingsKeyAdminIDs]),
			PricePerAccount: cfg.PricePerAccount,
			MaxAccounts:     cfg.MaxAccounts,
			MaxZipMB:        cfg.MaxZipBytes / 1000000,
			BotName:         s.botName(),
			HomeBannerURL:   cfg.HomeBannerURL,
			BannerUpload:    s.bannerEnabled(),
		},
	})
}

func (s *AdminServer) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PostFormValue("bot_token"))
	if token == "" || strings.HasPrefix(token, "Ã¢â‚¬Â¢Ã¢â‚¬Â¢") {
		token = ""
	}
	adminIDs := strings.TrimSpace(r.PostFormValue("admin_ids"))
	price := atoi64Default(r.PostFormValue("price_per_account"), 0)
	maxAccounts := atoiDefault(r.PostFormValue("max_accounts"), 0)
	maxZipMB := atoi64Default(r.PostFormValue("max_zip_mb"), 0)
	homeBanner := strings.TrimSpace(r.PostFormValue("home_banner_url"))

	uploaded := false
	urlBanner, uerr := s.storeBannerUpload(r)
	if uerr != nil {
		redirectFlash(w, r, "/settings", "err", uerr.Error())
		return
	}
	if urlBanner != "" {
		homeBanner = urlBanner
		uploaded = true
	}

	if price < 1000 {
		redirectFlash(w, r, "/settings", "err", "Harga per akun minimal Rp 1.000.")
		return
	}
	if maxAccounts < 1 || maxAccounts > 100 {
		redirectFlash(w, r, "/settings", "err", "Maksimal akun per order harus 1-100.")
		return
	}
	if maxZipMB < 1 {
		redirectFlash(w, r, "/settings", "err", "Batas ukuran ZIP minimal 1 MB.")
		return
	}
	if homeBanner != "" {
		if !strings.HasPrefix(homeBanner, "https://") && !strings.HasPrefix(homeBanner, "http://") {
			redirectFlash(w, r, "/settings", "err", "URL banner harus diawali http:// atau https://.")
			return
		}
		if len(homeBanner) > 1000 {
			redirectFlash(w, r, "/settings", "err", "URL banner terlalu panjang.")
			return
		}
	}

	if token != "" && token != s.app.cfg.BotToken {
		if err := s.app.reconnectBot(token); err != nil {
			redirectFlash(w, r, "/settings", "err", "Token bot tidak valid: "+err.Error())
			return
		}
	}

	values := map[string]string{
		settingsKeyAdminIDs:    adminIDs,
		settingsKeyPrice:       strconv.FormatInt(price, 10),
		settingsKeyMaxAccounts: strconv.Itoa(maxAccounts),
		settingsKeyMaxZipBytes: strconv.FormatInt(maxZipMB*1000000, 10),
		settingsKeyHomeBanner:  homeBanner,
	}
	if token != "" {
		values[settingsKeyBotToken] = token
	}
	if err := s.store.SetSettings(r.Context(), values); err != nil {
		redirectFlash(w, r, "/settings", "err", "Gagal menyimpan: "+err.Error())
		return
	}
	applySettings(&s.app.cfg, values)
	msg := "Pengaturan disimpan. Bot aktif: @" + s.botName()
	if uploaded {
		msg += ". Banner baru diupload ke RustFS."
	}
	redirectFlash(w, r, "/settings", "ok", msg)
}

func (s *AdminServer) handleTextsGet(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.LoadSettings(r.Context())
	if err != nil {
		s.renderError(w, r, "Gagal memuat teks bot: "+err.Error())
		return
	}
	views := make([]textView, 0, len(textDefs))
	for _, d := range textDefs {
		v := strings.TrimSpace(settings[d.Key])
		if v == "" {
			v = d.Default
		}
		views = append(views, textView{
			Key:          d.Key,
			Label:        d.Label,
			Description:  d.Description,
			Placeholders: d.Placeholders,
			Value:        v,
		})
	}
	s.render(w, r, "texts", &pageData{
		Title:  "Teks Bot",
		Active: "texts",
		Texts:  views,
	})
}

func (s *AdminServer) handleTextsPost(w http.ResponseWriter, r *http.Request) {
	values := make(map[string]string, len(textDefs))
	for _, d := range textDefs {
		v := strings.TrimSpace(r.PostFormValue(d.Key))
		if len(v) > 4000 {
			redirectFlash(w, r, "/texts", "err", "Teks \""+d.Label+"\" terlalu panjang (maks. 4000 karakter).")
			return
		}
		if d.Key == "msg_delivery" && len(v) > 900 {
			redirectFlash(w, r, "/texts", "err", "Caption file maksimal 900 karakter agar aman di Telegram.")
			return
		}
		if d.Key == "msg_qris" && len(v) > 900 {
			redirectFlash(w, r, "/texts", "err", "Teks QRIS maksimal 900 karakter karena dikirim sebagai caption foto.")
			return
		}
		if err := validateBotText(v); err != nil && v != "" {
			redirectFlash(w, r, "/texts", "err", "Template \""+d.Label+"\" tidak valid: "+err.Error())
			return
		}
		values[d.Key] = v
	}
	if err := s.store.SetSettings(r.Context(), values); err != nil {
		redirectFlash(w, r, "/texts", "err", "Gagal menyimpan: "+err.Error())
		return
	}
	s.app.applyTextValues(values)
	redirectFlash(w, r, "/texts", "ok", "Teks bot disimpan dan langsung dipakai.")
}

func (s *AdminServer) handlePasswordGet(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "password", &pageData{
		Title:  "Ganti Password",
		Active: "password",
	})
}

func (s *AdminServer) handlePasswordPost(w http.ResponseWriter, r *http.Request) {
	sess, _ := r.Context().Value(sessionCtxKey{}).(*AdminSession)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	current := r.PostFormValue("current_password")
	newPass := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")
	if newPass != confirm {
		redirectFlash(w, r, "/password", "err", "Konfirmasi password tidak sama.")
		return
	}
	if len(newPass) < 8 {
		redirectFlash(w, r, "/password", "err", "Password baru minimal 8 karakter.")
		return
	}
	user, err := s.store.AdminUserByUsername(r.Context(), sess.Username)
	if err != nil || !verifyPassword(user.PasswordHash, current) {
		redirectFlash(w, r, "/password", "err", "Password saat ini salah.")
		return
	}
	hash, err := hashPassword(newPass)
	if err != nil {
		redirectFlash(w, r, "/password", "err", "Gagal memproses password.")
		return
	}
	if err := s.store.UpdateAdminPassword(r.Context(), user.ID, hash); err != nil {
		redirectFlash(w, r, "/password", "err", "Gagal menyimpan password: "+err.Error())
		return
	}
	_ = s.store.DeleteOtherAdminSessions(r.Context(), user.ID, sess.Token)
	redirectFlash(w, r, "/password", "ok", "Password berhasil diganti.")
}

func (s *AdminServer) loadOrder(w http.ResponseWriter, r *http.Request) (*Order, bool) {
	o, err := s.store.GetOrder(r.Context(), r.PathValue("id"))
	if isNoRows(err) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		http.Error(w, "gagal memuat order", http.StatusInternalServerError)
		return nil, false
	}
	return o, true
}

func (s *AdminServer) dispatchDelivery(orderID string, verify bool) {
	o, err := s.store.GetOrder(context.Background(), orderID)
	if err != nil {
		log.Printf("panel admin: ambil order %s: %v", orderID, err)
		return
	}
	chatID := o.ChatID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		s.app.deliverOrder(ctx, chatID, 0, orderID, verify)
	}()
}

func (s *AdminServer) botName() string {
	if s.app.api != nil && s.app.api.Self.UserName != "" {
		return s.app.api.Self.UserName
	}
	return "-"
}

func (s *AdminServer) render(w http.ResponseWriter, r *http.Request, name string, data *pageData) {
	sess, _ := r.Context().Value(sessionCtxKey{}).(*AdminSession)
	if sess != nil {
		data.User = sess.Username
		data.CSRF = sess.CSRF
	}
	data.BotName = s.botName()
	data.Flash, data.FlashKind = flashFrom(r)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("panel admin: render %s: %v", name, err)
		http.Error(w, "gagal render halaman", http.StatusInternalServerError)
	}
}

func (s *AdminServer) renderError(w http.ResponseWriter, r *http.Request, msg string) {
	redirectFlash(w, r, "/", "err", msg)
}

func (s *AdminServer) renderLogin(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.loginTmpl.ExecuteTemplate(w, "login.html", &pageData{Title: "Masuk", Err: msg}); err != nil {
		log.Printf("panel admin: render login: %v", err)
	}
}

func flashFrom(r *http.Request) (string, string) {
	if v := r.URL.Query().Get("ok"); v != "" {
		return v, "success"
	}
	if v := r.URL.Query().Get("err"); v != "" {
		return v, "danger"
	}
	return "", ""
}

func redirectFlash(w http.ResponseWriter, r *http.Request, path, kind, msg string) {
	q := url.Values{}
	q.Set(kind, msg)
	http.Redirect(w, r, path+"?"+q.Encode(), http.StatusSeeOther)
}

func ordersURL(q, status, invoice string, page int) string {
	if page < 1 {
		page = 1
	}
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if status != "" {
		v.Set("status", status)
	}
	if invoice != "" {
		v.Set("invoice", invoice)
	}
	v.Set("page", strconv.Itoa(page))
	return "/orders?" + v.Encode()
}

func invoiceTextID(status string) string {
	switch status {
	case "paid":
		return "Lunas"
	case "review":
		return "Review"
	case "expired":
		return "Kedaluwarsa"
	default:
		return "Menunggu pembayaran"
	}
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "Ã¢â‚¬Â¢Ã¢â‚¬Â¢Ã¢â‚¬Â¢Ã¢â‚¬Â¢"
	}
	return "Ã¢â‚¬Â¢Ã¢â‚¬Â¢Ã¢â‚¬Â¢Ã¢â‚¬Â¢Ã¢â‚¬Â¢Ã¢â‚¬Â¢Ã¢â‚¬Â¢Ã¢â‚¬Â¢" + s[len(s)-4:]
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func atoi64Default(s string, def int64) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return def
	}
	return n
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
				"script-src 'self' https://cdn.jsdelivr.net; img-src 'self' data: https:; "+
				"font-src https://cdn.jsdelivr.net; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
