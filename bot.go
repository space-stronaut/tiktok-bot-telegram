package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/minio/minio-go/v7"
)

const (
	stepCustom = "custom"
	sessionTTL = 15 * time.Minute
)

type session struct {
	date    string
	step    string
	expires time.Time
}

type App struct {
	cfg      Config
	store    *Store
	pay      *PanzzPay
	api      *tgbotapi.BotAPI
	loc      *time.Location
	mu       sync.Mutex
	sessions map[int64]*session
	busy     map[int64]bool
	ui       map[int64]int
	uiVer    map[int64]int
	uiOrder  map[int64]string
	qris     map[string]qrisRef
	pollers  map[string]bool
	textsMu  sync.RWMutex
	texts    map[string]string
}

type qrisRef struct {
	chatID int64
	msgID  int
}

func newApp(cfg Config, store *Store, pay *PanzzPay, api *tgbotapi.BotAPI, loc *time.Location) *App {
	return &App{
		cfg:      cfg,
		store:    store,
		pay:      pay,
		api:      api,
		loc:      loc,
		sessions: make(map[int64]*session),
		busy:     make(map[int64]bool),
		ui:       make(map[int64]int),
		uiVer:    make(map[int64]int),
		uiOrder:  make(map[int64]string),
		qris:     make(map[string]qrisRef),
		pollers:  make(map[string]bool),
		texts:    make(map[string]string),
	}
}

// reconnectBot mengganti token bot secara live tanpa restart container.
func (a *App) reconnectBot(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token bot kosong")
	}
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return err
	}
	api.Client = &http.Client{Timeout: 15 * time.Minute}

	old := a.api
	a.api = api
	a.cfg.BotToken = token
	if old != nil {
		old.StopReceivingUpdates()
	}
	log.Printf("bot token diperbarui: @%s", api.Self.UserName)
	go a.run()
	a.setupBotMenu()
	return nil
}

func (a *App) setupBotMenu() {
	cmds := []tgbotapi.BotCommand{
		{Command: "start", Description: "Menu utama"},
		{Command: "stok", Description: "Lihat stok akun"},
		{Command: "cancel", Description: "Kembali ke menu"},
		{Command: "help", Description: "Bantuan"},
		{Command: "id", Description: "Lihat chat ID"},
	}
	if _, err := a.api.Request(tgbotapi.NewSetMyCommands(cmds...)); err != nil {
		log.Printf("set menu bot: %v", err)
	}
}

func (a *App) run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	for update := range a.api.GetUpdatesChan(u) {
		if update.CallbackQuery != nil {
			a.handleCallback(update.CallbackQuery)
			continue
		}
		msg := update.Message
		if msg == nil || msg.Chat == nil || msg.Chat.Type != "private" {
			continue
		}
		chatID := msg.Chat.ID
		if msg.IsCommand() {
			a.deleteMessage(chatID, msg.MessageID)
			a.clearSession(chatID)
			switch msg.Command() {
			case "start", "stok":
				a.handleStart(chatID)
			case "cancel":
				a.deleteUI(chatID)
				a.showHome(chatID, 0)
			case "help":
				a.deleteUI(chatID)
				a.showHelp(chatID, 0)
			case "id":
				a.deleteUI(chatID)
				a.showID(chatID, 0)
			default:
				a.deleteUI(chatID)
				a.showHelp(chatID, 0)
			}
			continue
		}
		a.handleText(msg)
	}
}

func (a *App) maintenance(ctx context.Context) {
	a.maintenanceOnce(ctx)
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.maintenanceOnce(ctx)
		}
	}
}

func (a *App) maintenanceOnce(ctx context.Context) {
	if err := a.store.ReleaseStale(ctx); err != nil {
		log.Printf("release stale: %v", err)
	}
	if res, err := a.store.SyncReconcile(ctx); err != nil {
		log.Printf("sync: %v", err)
	} else {
		log.Printf("sync: %d akun", res.Total)
	}
}

const helpText = `<b>Bot Akun TikTok</b>

/start — menu utama
/cancel — kembali ke menu
/id — lihat chat ID kamu

Cara beli:
1. Pilih <b>Beli Akun</b>
2. Pilih tanggal & jumlah
3. Konfirmasi, bayar via QRIS
4. File ZIP akun dikirim otomatis setelah pembayaran terverifikasi

Gunakan tombol <b>Pesanan Saya</b> untuk melihat riwayat.`

func (a *App) handleStart(chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := a.store.Sync(ctx); err != nil {
		log.Printf("sync saat /start: %v", err)
	}
	a.deleteUI(chatID)
	a.showHome(chatID, 0)
}

func (a *App) showHome(chatID int64, msgID int) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.clearUIOrder(chatID)

	total := 0
	if dates, err := a.store.Dates(ctx); err == nil {
		for _, d := range dates {
			total += d.Count
		}
	}
	render := func(star string) string {
		return a.message("msg_home", map[string]any{
			"Star":  star,
			"Stock": total,
			"Price": rupiah(a.cfg.PricePerAccount),
		})
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
			tgbotapi.NewInlineKeyboardButtonData("📦 Pesanan Saya", "myorders"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔎 Cek Status", "statusmenu"),
			tgbotapi.NewInlineKeyboardButtonData("❓ Bantuan", "help"),
		),
		a.adminContactRows(),
	)
	id, photo := a.showHomeMessage(chatID, msgID, render("✨"), &kb)
	if id != 0 {
		a.animateHome(chatID, id, photo, &kb, render)
	}
	return id
}

func (a *App) showHomeMessage(chatID int64, msgID int, text string, kb *tgbotapi.InlineKeyboardMarkup) (int, bool) {
	banner := strings.TrimSpace(a.cfg.HomeBannerURL)
	var sent tgbotapi.Message
	var err error
	photo := false
	if banner != "" {
		p := tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(banner))
		p.Caption = text
		p.ParseMode = tgbotapi.ModeHTML
		p.ReplyMarkup = kb
		sent, err = a.api.Send(p)
		if err != nil {
			log.Printf("banner home gagal: %v", err)
		} else {
			photo = true
		}
	}
	if sent.MessageID == 0 {
		msg := tgbotapi.NewMessage(chatID, text)
		msg.ParseMode = tgbotapi.ModeHTML
		msg.ReplyMarkup = kb
		sent, err = a.api.Send(msg)
		if err != nil {
			log.Printf("kirim home: %v", err)
			return 0, false
		}
	}
	if msgID != 0 && msgID != sent.MessageID {
		a.deleteMessage(chatID, msgID)
	}
	a.setUI(chatID, sent.MessageID)
	return sent.MessageID, photo
}

func (a *App) animateHome(chatID int64, msgID int, photo bool, kb *tgbotapi.InlineKeyboardMarkup, render func(string) string) {
	ver := a.uiVersion(chatID)
	go func() {
		for _, frame := range []string{"💫", "⭐", "🌟", "✨", "💫", "⭐", "✨"} {
			time.Sleep(1500 * time.Millisecond)
			if a.uiVersion(chatID) != ver || a.getUI(chatID) != msgID {
				return
			}
			text := render(frame)
			var err error
			if photo {
				e := tgbotapi.NewEditMessageCaption(chatID, msgID, text)
				e.ParseMode = tgbotapi.ModeHTML
				e.ReplyMarkup = kb
				_, err = a.api.Send(e)
			} else {
				e := tgbotapi.NewEditMessageText(chatID, msgID, text)
				e.ParseMode = tgbotapi.ModeHTML
				e.ReplyMarkup = kb
				_, err = a.api.Send(e)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "message is not modified") {
					log.Printf("animasi home %d: %v", chatID, err)
					return
				}
			}
		}
	}()
}

func (a *App) showHelp(chatID int64, msgID int) int {
	a.clearUIOrder(chatID)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
		a.adminContactRows(),
	)
	return a.show(chatID, msgID, a.message("msg_help", map[string]any{
		"Price": rupiah(a.cfg.PricePerAccount),
	}), &kb)
}

func (a *App) showID(chatID int64, msgID int) int {
	a.clearUIOrder(chatID)
	text := fmt.Sprintf("🆔 Chat ID: <code>%d</code>", chatID)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
	return a.show(chatID, msgID, text, &kb)
}

func (a *App) showShop(chatID int64, msgID int) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a.clearUIOrder(chatID)

	if _, err := a.store.Sync(ctx); err != nil {
		log.Printf("sync shop: %v", err)
	}
	dates, err := a.store.Dates(ctx)
	if err != nil {
		return a.showError(chatID, msgID, "❌ Gagal membaca stok. Coba lagi nanti.")
	}
	if len(dates) == 0 {
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		))
		return a.show(chatID, msgID, "📭 Belum ada akun yang tersedia.", &kb)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, d := range dates {
		label := fmt.Sprintf("📅 %s (%d Hari)", formatDateFullID(d.Date), accountAgeDays(d.Date, a.loc))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, "date:"+d.Date.Format("2006-01-02")),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
	))
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	return a.show(chatID, msgID, a.message("msg_shop", map[string]any{
		"Price": rupiah(a.cfg.PricePerAccount),
	}), &kb)
}

func (a *App) onDate(chatID int64, msgID int, date string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.clearUIOrder(chatID)

	count, err := a.store.Available(ctx, date)
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca stok. Coba lagi.")
		return
	}
	if count == 0 {
		if _, err := a.store.Sync(ctx); err == nil {
			count, _ = a.store.Available(ctx, date)
		}
	}
	if count == 0 {
		a.showError(chatID, msgID, "❌ Stok untuk tanggal itu sudah habis.")
		return
	}

	max := count
	if a.cfg.MaxAccounts < max {
		max = a.cfg.MaxAccounts
	}

	presets := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	var rows [][]tgbotapi.InlineKeyboardButton
	if count > 10 {
		var row []tgbotapi.InlineKeyboardButton
		for _, p := range presets {
			if p > max {
				continue
			}
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(
				strconv.Itoa(p), fmt.Sprintf("qty:%s:%d", date, p)))
			if len(row) == 5 {
				rows = append(rows, row)
				row = nil
			}
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Custom jumlah", "custom:"+date),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)

	text := a.message("msg_date", map[string]any{
		"Date":  formatDateString(date, a.loc),
		"Stock": count,
		"Price": rupiah(a.cfg.PricePerAccount),
	})
	a.show(chatID, msgID, text, &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) onCustom(chatID int64, msgID int, date, notice string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.clearUIOrder(chatID)

	count, err := a.store.Available(ctx, date)
	if err != nil || count == 0 {
		a.showError(chatID, msgID, "❌ Stok untuk tanggal itu sudah habis.")
		return
	}
	max := count
	if a.cfg.MaxAccounts < max {
		max = a.cfg.MaxAccounts
	}

	a.setSession(chatID, &session{
		date:    date,
		step:    stepCustom,
		expires: time.Now().Add(sessionTTL),
	})
	text := a.message("msg_custom", map[string]any{
		"Date":  formatDateString(date, a.loc),
		"Stock": count,
		"Max":   max,
	})
	text += "\n\n⚠️ Minimal pesanan <b>10 akun</b>."
	if max < 10 {
		text += fmt.Sprintf("\n❌ Stok tanggal ini hanya %d — kurang dari minimal.", max)
	}
	if notice != "" {
		text += "\n" + notice
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "home"),
	))
	a.show(chatID, msgID, text, &kb)
}

func (a *App) handleText(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	a.deleteMessage(chatID, msg.MessageID)

	msgID := a.getUI(chatID)
	s := a.getSession(chatID)
	if s == nil || s.step != stepCustom {
		a.showHome(chatID, msgID)
		return
	}

	qty, err := strconv.Atoi(strings.TrimSpace(msg.Text))
	if err != nil || qty < 1 {
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "home"),
		))
		a.show(chatID, msgID, "❌ Jumlah tidak valid. Ketik angka, contoh: 7", &kb)
		return
	}
	if qty < 10 {
		a.onCustom(chatID, msgID, s.date, "❌ Minimal pesanan <b>10 akun</b> — ketik jumlah yang lebih besar.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	available, err := a.store.Available(ctx, s.date)
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca stok. Coba lagi.")
		return
	}
	if qty > a.cfg.MaxAccounts {
		a.clearSession(chatID)
		a.showError(chatID, msgID, fmt.Sprintf("❌ Maksimal %d akun per pesanan.", a.cfg.MaxAccounts))
		return
	}
	if qty > available {
		a.clearSession(chatID)
		a.showError(chatID, msgID, fmt.Sprintf("❌ Stok tidak cukup. Tersedia %d akun.", available))
		return
	}

	a.clearSession(chatID)
	a.showConfirmation(chatID, msgID, s.date, qty)
}

func (a *App) showConfirmation(chatID int64, msgID int, date string, qty int) int {
	a.clearUIOrder(chatID)
	total := int64(qty) * a.cfg.PricePerAccount
	fee := feeEstimate(total)
	token := newToken()
	text := a.message("msg_confirm", map[string]any{
		"Date":       formatDateString(date, a.loc),
		"Qty":        qty,
		"Price":      rupiah(a.cfg.PricePerAccount),
		"Total":      rupiah(total),
		"Fee":        rupiah(fee),
		"GrandTotal": rupiah(total + fee),
	})

	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💳 Bayar", fmt.Sprintf("pay:%s:%d:%s", date, qty, token)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "home"),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
	return a.show(chatID, msgID, text, &kb)
}

func (a *App) handleCallback(cq *tgbotapi.CallbackQuery) {
	if cq.Message == nil || cq.Message.Chat == nil {
		return
	}
	chatID := cq.Message.Chat.ID
	_, _ = a.api.Request(tgbotapi.NewCallback(cq.ID, ""))

	msgID := a.getUI(chatID)
	if msgID == 0 {
		msgID = cq.Message.MessageID
	}

	data := cq.Data
	switch {
	case data == "home":
		a.clearSession(chatID)
		a.showHome(chatID, msgID)
	case data == "shop":
		a.showShop(chatID, msgID)
	case data == "help":
		a.showHelp(chatID, msgID)
	case data == "myorders":
		a.showMyOrders(chatID, msgID)
	case data == "statusmenu":
		a.showStatusMenu(chatID, msgID)
	case strings.HasPrefix(data, "date:"):
		a.onDate(chatID, msgID, strings.TrimPrefix(data, "date:"))
	case strings.HasPrefix(data, "custom:"):
		a.onCustom(chatID, msgID, strings.TrimPrefix(data, "custom:"), "")
	case strings.HasPrefix(data, "qty:"):
		parts := strings.SplitN(strings.TrimPrefix(data, "qty:"), ":", 2)
		if len(parts) != 2 {
			return
		}
		qty, err := strconv.Atoi(parts[1])
		if err != nil || qty < 1 {
			return
		}
		a.onPresetQty(chatID, msgID, parts[0], qty)
	case strings.HasPrefix(data, "pay:"):
		parts := strings.Split(strings.TrimPrefix(data, "pay:"), ":")
		if len(parts) != 3 {
			return
		}
		qty, err := strconv.Atoi(parts[1])
		if err != nil || qty < 1 || parts[2] == "" {
			return
		}
		username := ""
		if cq.From != nil {
			username = cq.From.UserName
		}
		a.onPay(chatID, msgID, username, parts[0], qty, parts[2])
	case strings.HasPrefix(data, "paycheck:"):
		a.checkPayment(chatID, msgID, strings.TrimPrefix(data, "paycheck:"))
	case strings.HasPrefix(data, "payretry:"):
		a.retryInvoice(chatID, msgID, strings.TrimPrefix(data, "payretry:"))
	case strings.HasPrefix(data, "qr:"):
		a.resendQRIS(chatID, strings.TrimPrefix(data, "qr:"))
	case strings.HasPrefix(data, "deliver:"):
		a.onDeliver(chatID, msgID, strings.TrimPrefix(data, "deliver:"))
	case strings.HasPrefix(data, "status:"):
		a.showOrderStatus(chatID, msgID, strings.TrimPrefix(data, "status:"))
	case strings.HasPrefix(data, "cancel:"):
		a.showCancelResult(chatID, msgID, strings.TrimPrefix(data, "cancel:"))
	}
}

func (a *App) onPresetQty(chatID int64, msgID int, date string, qty int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	available, err := a.store.Available(ctx, date)
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca stok. Coba lagi.")
		return
	}
	if qty > available {
		a.showError(chatID, msgID, fmt.Sprintf("❌ Stok tidak cukup. Tersedia %d akun.", available))
		return
	}
	if qty > a.cfg.MaxAccounts {
		a.showError(chatID, msgID, fmt.Sprintf("❌ Maksimal %d akun per pesanan.", a.cfg.MaxAccounts))
		return
	}
	a.clearSession(chatID)
	a.showConfirmation(chatID, msgID, date, qty)
}

func (a *App) onPay(chatID int64, msgID int, username, date string, qty int, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	available, err := a.store.Available(ctx, date)
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca stok. Coba lagi.")
		return
	}
	if qty > available {
		a.showError(chatID, msgID, fmt.Sprintf("❌ Stok tidak cukup. Tersedia %d akun.", available))
		return
	}
	if qty > a.cfg.MaxAccounts {
		a.showError(chatID, msgID, fmt.Sprintf("❌ Maksimal %d akun per pesanan.", a.cfg.MaxAccounts))
		return
	}
	if !a.tryBusy(chatID) {
		a.showError(chatID, msgID, "⏳ Masih ada pesanan yang sedang diproses. Tunggu sebentar.")
		return
	}
	a.clearSession(chatID)
	a.show(chatID, msgID, "⏳ Membuat pesanan...", emptyKB())
	go a.process(chatID, msgID, username, date, qty, token)
}

func (a *App) process(chatID int64, msgID int, username, date string, qty int, token string) {
	defer a.clearBusy(chatID)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if existing, err := a.store.PendingOrderByChat(ctx, chatID); err == nil {
		if existing.InvoiceExpiresAt.Valid && existing.InvoiceExpiresAt.Time.Before(time.Now().Add(-5*time.Minute)) && existing.InvoiceStatus != "review" {
			a.expireOrder(ctx, existing, msgID)
		} else {
			log.Printf("chat %d masih punya order pending %s, pakai invoice itu", chatID, existing.ID)
			a.showPayment(chatID, msgID, existing, true)
			return
		}
	}

	orderID := fmt.Sprintf("ORD-%s-%d-%s", time.Now().Format("20060102-150405"), chatID, token[:4])
	idemKey := fmt.Sprintf("%d:%s", chatID, token)

	_, _, duplicate, err := a.store.Reserve(ctx, orderID, idemKey, chatID, username, date, qty, a.cfg.MaxZipBytes, a.cfg.PricePerAccount)
	if err != nil {
		switch {
		case errors.Is(err, ErrInsufficient):
			a.showError(chatID, msgID, "❌ Stok tidak cukup. Ketik /start untuk melihat stok terbaru.")
		case errors.Is(err, ErrTooLarge):
			a.showError(chatID, msgID, "❌ Pesanan terlalu besar untuk dikirim lewat Telegram. Kurangi jumlah akun.")
		default:
			log.Printf("reserve %s: %v", orderID, err)
			a.showError(chatID, msgID, "❌ Gagal memproses pesanan. Coba lagi nanti.")
		}
		return
	}
	if duplicate != nil {
		a.showPayment(chatID, msgID, duplicate, true)
		return
	}
	log.Printf("order %s: reservasi %d akun %s untuk chat %d", orderID, qty, date, chatID)

	o, err := a.store.GetOrder(ctx, orderID)
	if err != nil {
		log.Printf("ambil order %s: %v", orderID, err)
		a.showError(chatID, msgID, "❌ Gagal menampilkan pesanan. Coba cek di Pesanan Saya.")
		return
	}
	a.showPayment(chatID, msgID, o, false)
}

func (a *App) showPayment(chatID int64, msgID int, o *Order, existed bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := a.ensureInvoice(ctx, o); err != nil {
		log.Printf("invoice %s: %v", o.ID, err)
		a.setUIOrder(chatID, o.ID)
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔄 Coba Lagi", "payretry:"+o.ID),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("❌ Batalkan", "cancel:"+o.ID),
				tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
			),
		)
		a.show(chatID, msgID, "❌ Gagal membuat QRIS. Stok tetap direservasi sementara.\nKlik <b>Coba Lagi</b> atau tunggu beberapa saat.", &kb)
		a.notifyAdmins(fmt.Sprintf("⚠️ <b>Gagal membuat invoice</b>\nOrder: <code>%s</code>\nChat: <code>%d</code>\nError: %s",
			o.ID, chatID, html.EscapeString(err.Error())))
		return
	}

	switch o.InvoiceStatus {
	case "review":
		a.showReviewWindow(chatID, msgID, o)
	case "paid":
		a.deliverPaid(ctx, chatID, msgID, o.ID)
		return
	default:
		a.showQRISWindow(chatID, msgID, o, existed)
	}
	a.startPoller(o.ID)
}

func (a *App) showReviewWindow(chatID int64, msgID int, o *Order) int {
	a.setUIOrder(chatID, o.ID)
	text := a.message("msg_review", map[string]any{
		"OrderID": o.ID,
		"Invoice": o.InvoiceID,
		"Qty":     o.Quantity,
		"Total":   rupiah(o.GatewayTotal),
	})
	kb := a.reviewKeyboard(o.ID)
	return a.show(chatID, msgID, text, &kb)
}

func (a *App) reviewKeyboard(orderID string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Cek Status", "paycheck:"+orderID),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
		a.adminContactRows(),
	)
}

func (a *App) ensureInvoice(ctx context.Context, o *Order) (*Invoice, error) {
	if o.InvoiceID != "" {
		return &Invoice{
			InvoiceID: o.InvoiceID,
			Amount:    o.TotalPrice,
			Fee:       o.PaymentFee,
			Total:     o.GatewayTotal,
			QRISImage: o.QRISURL,
			Status:    o.InvoiceStatus,
		}, nil
	}

	var inv *Invoice
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		inv, err = a.pay.CreateInvoice(ctx, o.TotalPrice, o.ID)
		if err == nil {
			break
		}
		log.Printf("create invoice %s (percobaan %d): %v", o.ID, attempt+1, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if err != nil {
		return nil, err
	}

	expires := parseGatewayTime(inv.ExpiredAt, a.loc)
	var saveErr error
	for attempt := 0; attempt < 3; attempt++ {
		if saveErr = a.store.SetInvoice(ctx, o.ID, inv, expires); saveErr == nil {
			break
		}
		log.Printf("simpan invoice %s (percobaan %d): %v", o.ID, attempt+1, saveErr)
		time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
	}
	if saveErr != nil {
		return nil, saveErr
	}

	o.InvoiceID = inv.InvoiceID
	o.InvoiceStatus = paymentStatus(inv.Status)
	o.PaymentFee = inv.Fee
	o.GatewayTotal = inv.Total
	o.QRISURL = inv.QRISImage
	if !expires.IsZero() {
		o.InvoiceExpiresAt = sql.NullTime{Time: expires.UTC(), Valid: true}
	}
	log.Printf("invoice order %s: id=%s amount=%d fee=%d total=%d", o.ID, inv.InvoiceID, inv.Amount, inv.Fee, inv.Total)
	return inv, nil
}

func (a *App) qrisMessage(o *Order, existed bool) (string, *tgbotapi.InlineKeyboardMarkup) {
	total := o.GatewayTotal
	if total == 0 {
		total = o.TotalPrice + o.PaymentFee
	}
	fee := o.PaymentFee
	if fee == 0 {
		fee = feeEstimate(o.TotalPrice)
	}
	expires := "mengikuti provider"
	if o.InvoiceExpiresAt.Valid {
		expires = o.InvoiceExpiresAt.Time.In(a.loc).Format("02 Jan 2006 15:04")
	}
	head := "💳 <b>Pembayaran QRIS</b>"
	if existed {
		head = "💳 <b>QRIS Pesanan</b>"
	}
	text := a.message("msg_qris", map[string]any{
		"Head":    head,
		"OrderID": o.ID,
		"Date":    formatDateID(o.Date),
		"Qty":     o.Quantity,
		"Price":   rupiah(o.TotalPrice),
		"Fee":     rupiah(fee),
		"Total":   rupiah(total),
		"Expires": expires,
	})
	kb := a.qrisKeyboard(o.ID)
	return text, &kb
}

func (a *App) showQRISWindow(chatID int64, msgID int, o *Order, existed bool) int {
	a.setUIOrder(chatID, o.ID)
	text, kb := a.qrisMessage(o, existed)

	if a.getQRIS(o.ID) != 0 {
		a.deleteQRIS(o.ID)
	}
	id := a.sendQRISPhoto(chatID, o, text, kb)
	if id == 0 {
		return a.show(chatID, msgID, text, kb)
	}
	if msgID != 0 && msgID != id {
		a.deleteMessage(chatID, msgID)
	}
	a.setUI(chatID, id)
	return id
}

// adminContactRows mengembalikan baris tombol kontak admin, atau nil jika
// URL kontak belum diatur (mis. bot client yang belum punya ID admin).
func (a *App) adminContactRows() []tgbotapi.InlineKeyboardButton {
	url := strings.TrimSpace(a.cfg.AdminContactURL)
	if url == "" {
		return nil
	}
	return tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonURL("💬 Hubungi Admin", url))
}

func (a *App) qrisKeyboard(orderID string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Cek Status", "paycheck:"+orderID),
			tgbotapi.NewInlineKeyboardButtonData("🖼️ Tampilkan QRIS", "qr:"+orderID),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Batalkan", "cancel:"+orderID),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
		a.adminContactRows(),
	)
}

func (a *App) checkPayment(chatID int64, msgID int, orderID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	o, err := a.store.GetOrder(ctx, orderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.ChatID != chatID) {
		a.showError(chatID, msgID, "❌ Pesanan tidak ditemukan.")
		return
	}
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca pesanan.")
		return
	}
	switch o.Status {
	case "sent":
		a.deleteQRIS(o.ID)
		kb := orderKeyboard(o.ID)
		a.show(chatID, msgID, "✅ Pesanan ini sudah lunas dan file sudah dikirim.\n\n"+orderCard(o, a.loc), &kb)
		return
	case "cancelled":
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		))
		a.show(chatID, msgID, "❌ Pesanan ini sudah dibatalkan / kedaluwarsa.", &kb)
		return
	case "paid", "processing":
		a.onDeliver(chatID, msgID, orderID)
		return
	}
	if o.InvoiceID == "" {
		a.showPayment(chatID, msgID, o, true)
		return
	}

	inv, err := a.pay.InvoiceStatus(ctx, o.InvoiceID)
	if err != nil {
		log.Printf("status invoice %s: %v", o.InvoiceID, err)
		a.setUIOrder(chatID, o.ID)
		kb := a.qrisKeyboard(o.ID)
		a.show(chatID, msgID, "⚠️ Gagal mengecek status ke gateway. Coba lagi sebentar.\n\n"+orderCard(o, a.loc), &kb)
		return
	}
	st := paymentStatus(inv.Status)
	_ = a.store.SetInvoiceStatus(ctx, o.ID, st)
	switch st {
	case "paid":
		a.deliverPaid(ctx, chatID, msgID, o.ID)
	case "expired":
		a.expireOrder(ctx, o, msgID)
	case "review":
		if o.InvoiceStatus != "review" {
			a.notifyAdmins(fmt.Sprintf("⚠️ <b>Pembayaran REVIEW</b>\nOrder: <code>%s</code>\nInvoice: <code>%s</code>\nChat: <code>%d</code>\nTotal: %s\nPerlu rekonsiliasi manual di dashboard gateway.",
				o.ID, o.InvoiceID, chatID, rupiah(o.GatewayTotal)))
		}
		a.showReviewWindow(chatID, msgID, o)
	default:
		a.showQRISWindow(chatID, msgID, o, true)
	}
}

func (a *App) onDeliver(chatID int64, msgID int, orderID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	o, err := a.store.GetOrder(ctx, orderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.ChatID != chatID) {
		a.showError(chatID, msgID, "❌ Pesanan tidak ditemukan.")
		return
	}
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca pesanan.")
		return
	}
	switch o.Status {
	case "sent":
		a.deleteQRIS(o.ID)
		kb := orderKeyboard(o.ID)
		a.show(chatID, msgID, "✅ File pesanan ini sudah dikirim sebelumnya. Tidak dikirim ulang.\n\n"+orderCard(o, a.loc), &kb)
		return
	case "cancelled":
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		))
		a.show(chatID, msgID, "❌ Pesanan ini sudah dibatalkan / kedaluwarsa.", &kb)
		return
	case "processing":
		a.showError(chatID, msgID, "⏳ Pesanan sedang diproses. Tunggu sebentar.")
		return
	case "paid":
		a.deliverPaid(ctx, chatID, msgID, o.ID)
		return
	}
	if o.InvoiceID == "" {
		a.showPayment(chatID, msgID, o, true)
		return
	}

	inv, err := a.pay.InvoiceStatus(ctx, o.InvoiceID)
	if err != nil {
		a.setUIOrder(chatID, o.ID)
		kb := a.qrisKeyboard(o.ID)
		a.show(chatID, msgID, "⚠️ Gagal mengecek pembayaran. Coba lagi.\n\n"+orderCard(o, a.loc), &kb)
		return
	}
	switch paymentStatus(inv.Status) {
	case "paid":
		a.deliverPaid(ctx, chatID, msgID, o.ID)
	case "expired":
		a.expireOrder(ctx, o, msgID)
	case "review":
		a.showReviewWindow(chatID, msgID, o)
	default:
		a.setUIOrder(chatID, o.ID)
		kb := a.qrisKeyboard(o.ID)
		a.show(chatID, msgID, "⏳ Pembayaran belum diterima. Selesaikan pembayaran QRIS terlebih dahulu.\n\n"+orderCard(o, a.loc), &kb)
	}
}

func (a *App) deliverPaid(ctx context.Context, chatID int64, msgID int, orderID string) {
	a.deliverOrder(ctx, chatID, msgID, orderID, true)
}

// deliverOrder mengirim file pesanan. Jika verify=true, status pembayaran
// dicek ulang ke gateway; verify=false dipakai admin untuk bypass review.
func (a *App) deliverOrder(ctx context.Context, chatID int64, msgID int, orderID string, verify bool) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	o, err := a.store.GetOrder(ctx, orderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.ChatID != chatID) {
		if msgID != 0 {
			a.showError(chatID, msgID, "❌ Pesanan tidak ditemukan.")
		}
		return
	}
	if err != nil {
		log.Printf("deliver %s: %v", orderID, err)
		return
	}
	switch o.Status {
	case "sent":
		a.deleteQRIS(o.ID)
		if a.getUIOrder(chatID) == o.ID && msgID != 0 {
			kb := orderKeyboard(o.ID)
			a.show(chatID, msgID, "✅ File pesanan ini sudah dikirim sebelumnya.\n\n"+orderCard(o, a.loc), &kb)
		}
		return
	case "cancelled":
		if msgID != 0 {
			a.showError(chatID, msgID, "❌ Pesanan sudah dibatalkan. Hubungi admin jika sudah terlanjur bayar.")
		}
		return
	}

	if verify && o.InvoiceID != "" {
		inv, err := a.pay.InvoiceStatus(ctx, o.InvoiceID)
		if err != nil {
			log.Printf("verifikasi bayar %s: %v", orderID, err)
			if msgID != 0 {
				a.setUIOrder(chatID, o.ID)
				kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🔄 Cek Status", "paycheck:"+o.ID),
					tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
				))
				a.show(chatID, msgID, "⚠️ Gagal memverifikasi pembayaran. Coba lagi.", &kb)
			}
			return
		}
		st := paymentStatus(inv.Status)
		if st == "expired" {
			a.expireOrder(ctx, o, msgID)
			return
		}
		if st == "review" {
			if msgID != 0 {
				a.showReviewWindow(chatID, msgID, o)
			}
			return
		}
		if st != "paid" {
			if msgID != 0 {
				a.setUIOrder(chatID, o.ID)
				kb := a.qrisKeyboard(o.ID)
				a.show(chatID, msgID, "⏳ Pembayaran belum diterima. File belum bisa dikirim.", &kb)
			}
			return
		}
		_ = a.store.SetInvoiceStatus(ctx, o.ID, "paid")
	}

	// Flash: pembayaran diterima, pesanan diproses. QRIS langsung dihapus
	// supaya pelanggan tidak membayar invoice yang sudah lunas.
	a.deleteQRIS(orderID)
	flashKB := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
	flashText := "✅ <b>Pembayaran diterima!</b>\n\n⏳ Pesanan sedang diproses — file akan dikirim otomatis di chat ini..."
	flashMsgID := 0
	if a.getUIOrder(chatID) == orderID && msgID == 0 {
		msgID = a.getUI(chatID)
	}
	if msgID != 0 {
		a.show(chatID, msgID, flashText, &flashKB)
		flashMsgID = msgID
	} else {
		flashMsgID = a.show(chatID, 0, flashText, &flashKB)
	}

	ok, err := a.store.ClaimDelivery(ctx, orderID, chatID)
	if err != nil {
		log.Printf("claim delivery %s: %v", orderID, err)
		a.showError(chatID, flashMsgID, "❌ Gagal memproses pesanan.")
		return
	}
	if !ok {
		o2, getErr := a.store.GetOrder(ctx, orderID)
		if getErr == nil && o2.Status == "sent" {
			a.deleteQRIS(o2.ID)
		} else {
			a.showError(chatID, flashMsgID, "⏳ Pesanan sedang diproses. Coba cek lagi sebentar.")
		}
		return
	}

	accounts, err := a.store.OrderAccounts(ctx, orderID)
	if err != nil || len(accounts) == 0 {
		_ = a.store.RevertDelivery(ctx, orderID)
		log.Printf("akun order %s: %v", orderID, err)
		a.notifyAdmins(fmt.Sprintf("⚠️ <b>Order lunas tapi akun gagal dibaca</b>\nOrder: <code>%s</code>\nChat: <code>%d</code>\nError: %s",
			orderID, chatID, html.EscapeString(fmt.Sprint(err))))
		a.showDeliveryError(chatID, flashMsgID, orderID, "❌ Gagal membaca daftar akun. Pembayaran tetap tercatat; klik Ambil File untuk coba lagi.")
		return
	}

	dir, err := os.MkdirTemp("", "tiktok-order-*")
	if err != nil {
		_ = a.store.RevertDelivery(ctx, orderID)
		a.showDeliveryError(chatID, msgID, orderID, "❌ Gagal membuat file sementara. Klik Ambil File untuk coba lagi.")
		return
	}
	defer os.RemoveAll(dir)

	// Telegram membatasi upload bot sekitar 50MB per dokumen; pecah
	// pesanan besar menjadi beberapa ZIP agar tetap bisa terkirim semua.
	const partLimit = 40 << 20 // 40 MB per bagian
	parts := planParts(accounts, partLimit)
	for i, part := range parts {
		name := fmt.Sprintf("%s-%s-%dakun", buyerFilePrefix(o), o.Date.Format("2006-01-02"), len(accounts))
		if len(parts) > 1 {
			name = fmt.Sprintf("%s-bagian%d-dari-%d", name, i+1, len(parts))
		}
		zipPath := filepath.Join(dir, name+".zip")
		if err := a.buildZip(ctx, part, zipPath); err != nil {
			_ = a.store.RevertDelivery(ctx, orderID)
			log.Printf("zip %s (bagian %d): %v", orderID, i+1, err)
			a.notifyAdmins(fmt.Sprintf("⚠️ <b>Order lunas tapi ZIP gagal dibuat</b>\nOrder: <code>%s</code>\nChat: <code>%d</code>\nError: %s",
				orderID, chatID, html.EscapeString(err.Error())))
			a.showDeliveryError(chatID, flashMsgID, orderID, "❌ Gagal menyiapkan file akun. Pembayaran tetap tercatat; klik Ambil File untuk coba lagi.")
			return
		}

		caption := a.message("msg_delivery", map[string]any{
			"OrderID": orderID,
			"Date":    formatDateID(o.Date),
			"Qty":     len(accounts),
			"Total":   rupiah(o.GatewayTotal),
		})
		if len(parts) > 1 {
			caption += fmt.Sprintf("\n\n📦 Bagian <b>%d / %d</b> — %d akun", i+1, len(parts), len(part))
		}
		doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(zipPath))
		doc.Caption = caption
		doc.ParseMode = tgbotapi.ModeHTML
		doc.ReplyMarkup = orderKeyboard(orderID)

		if _, err := a.api.Send(doc); err != nil {
			_ = a.store.RevertDelivery(ctx, orderID)
			log.Printf("kirim dokumen %s (bagian %d/%d): %v", orderID, i+1, len(parts), err)
			a.notifyAdmins(fmt.Sprintf("⚠️ <b>Order lunas tapi kirim file gagal</b>\nOrder: <code>%s</code>\nChat: <code>%d</code>\nBagian: %d/%d\nError: %s",
				orderID, chatID, i+1, len(parts), html.EscapeString(err.Error())))
			a.showDeliveryError(chatID, flashMsgID, orderID, "❌ Gagal mengirim file. Pembayaran tetap tercatat dan stok tidak dilepas; klik Ambil File untuk coba lagi.")
			return
		}
		// bebaskan tmpfs segera; jangan tunggu semua bagian selesai
		_ = os.Remove(zipPath)
	}

	finalized := false
	var finalErr error
	for i := 0; i < 5; i++ {
		if finalErr = a.store.Finalize(ctx, orderID); finalErr == nil {
			finalized = true
			break
		}
		log.Printf("finalize %s (percobaan %d): %v", orderID, i+1, finalErr)
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	if !finalized {
		a.notifyAdmins(fmt.Sprintf("⚠️ <b>File terkirim tapi finalize gagal</b>\nOrder: <code>%s</code>\nError: %s",
			orderID, html.EscapeString(fmt.Sprint(finalErr))))
	}
	_ = a.store.SetInvoiceStatus(ctx, orderID, "paid")
	a.deleteQRIS(orderID)
	// flash dihapus, diganti pesan selesai
	if flashMsgID != 0 {
		a.deleteMessage(chatID, flashMsgID)
	}
	a.clearUIOrder(chatID)
	doneKB := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📦 Pesanan Saya", "myorders"),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
	a.show(chatID, 0, fmt.Sprintf("✅ Pembayaran diterima. Order <code>%s</code> — %d akun terkirim.", orderID, len(accounts)), &doneKB)

	username := o.Username
	if username != "" {
		username = "@" + username
	} else {
		username = "-"
	}
	a.notifyAdmins(fmt.Sprintf("🛒 <b>Order lunas & terkirim</b>\nOrder: <code>%s</code>\nInvoice: <code>%s</code>\nUser: %s (<code>%d</code>)\nTanggal: %s\nJumlah: %d akun\nTotal bayar: %s\nUkuran: %.1f MB",
		orderID, o.InvoiceID, html.EscapeString(username), chatID, formatDateID(o.Date), len(accounts), rupiah(o.GatewayTotal), float64(o.AmountBytes)/1e6))
}

// planParts membagi akun menjadi beberapa bagian dengan jumlah akun yang
// rata — hanya bagian terakhir yang boleh berbeda — sambil memastikan
// tiap bagian tetap di bawah batas ukuran upload Telegram.
func planParts(accounts []Account, limit int64) [][]Account {
	if len(accounts) == 0 {
		return nil
	}
	var total int64
	for _, acc := range accounts {
		sz := acc.Size
		if sz <= 0 {
			sz = 1
		}
		total += sz
	}
	n := int((total + limit - 1) / limit)
	if n < 1 {
		n = 1
	}
	if n > len(accounts) {
		n = len(accounts)
	}
	for {
		parts := splitEven(accounts, n)
		if partsFit(parts, limit) || n >= len(accounts) {
			return parts
		}
		n++
	}
}

// splitEven memotong akun menjadi n bagian berurutan berukuran sama;
// sisa pembagian jatuh ke bagian terakhir (mis. 30 -> 10,10,10).
func splitEven(accounts []Account, n int) [][]Account {
	per := (len(accounts) + n - 1) / n
	var parts [][]Account
	for start := 0; start < len(accounts); start += per {
		end := start + per
		if end > len(accounts) {
			end = len(accounts)
		}
		parts = append(parts, accounts[start:end])
	}
	return parts
}

func partsFit(parts [][]Account, limit int64) bool {
	for _, p := range parts {
		var s int64
		for _, acc := range p {
			sz := acc.Size
			if sz <= 0 {
				sz = 1
			}
			s += sz
		}
		if s > limit {
			return false
		}
	}
	return true
}

func buyerFilePrefix(o *Order) string {
	name := strings.TrimSpace(o.Username)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		out = fmt.Sprintf("user%d", o.ChatID)
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func (a *App) showDeliveryError(chatID int64, msgID int, orderID, text string) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📥 Ambil File", "deliver:"+orderID),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
	if a.getUIOrder(chatID) != orderID {
		msgID = 0
	} else if msgID == 0 {
		msgID = a.getUI(chatID)
	}
	a.show(chatID, msgID, text, &kb)
}

func (a *App) sendQRISPhoto(chatID int64, o *Order, caption string, kb *tgbotapi.InlineKeyboardMarkup) int {
	if o.QRISURL == "" {
		return 0
	}
	photo := tgbotapi.NewPhoto(chatID, qrisFileData(o))
	photo.Caption = caption
	photo.ParseMode = tgbotapi.ModeHTML
	if kb != nil {
		photo.ReplyMarkup = kb
	}

	sent, err := a.api.Send(photo)
	if err != nil {
		log.Printf("kirim qris url %s: %v, coba unduh", o.ID, err)
		data, derr := downloadQRIS(o.QRISURL)
		if derr != nil {
			log.Printf("unduh qris %s: %v", o.ID, derr)
			return 0
		}
		photo = tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{Name: "qris-" + o.ID + ".png", Bytes: data})
		photo.Caption = caption
		photo.ParseMode = tgbotapi.ModeHTML
		if kb != nil {
			photo.ReplyMarkup = kb
		}
		sent, err = a.api.Send(photo)
		if err != nil {
			log.Printf("kirim qris upload %s: %v", o.ID, err)
			return 0
		}
	}
	a.setQRIS(chatID, o.ID, sent.MessageID)
	return sent.MessageID
}

// qrisFileData menyiapkan file QRIS untuk Telegram. QRIS PanzzPay berupa
// data URL base64, sedangkan invoice lama bisa berupa URL biasa.
func qrisFileData(o *Order) tgbotapi.RequestFileData {
	if data, ok := decodeDataURL(o.QRISURL); ok {
		return tgbotapi.FileBytes{Name: "qris-" + o.ID + ".png", Bytes: data}
	}
	return tgbotapi.FileURL(o.QRISURL)
}

func decodeDataURL(raw string) ([]byte, bool) {
	if !strings.HasPrefix(raw, "data:") {
		return nil, false
	}
	const marker = "base64,"
	idx := strings.Index(raw, marker)
	if idx < 0 {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(raw[idx+len(marker):])
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

func downloadQRIS(rawURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func (a *App) retryInvoice(chatID int64, msgID int, orderID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	o, err := a.store.GetOrder(ctx, orderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.ChatID != chatID) {
		a.showError(chatID, msgID, "❌ Pesanan tidak ditemukan.")
		return
	}
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca pesanan.")
		return
	}
	if o.Status != "pending" {
		a.showOrderStatus(chatID, msgID, orderID)
		return
	}
	a.showPayment(chatID, msgID, o, true)
}

func (a *App) resendQRIS(chatID int64, orderID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	o, err := a.store.GetOrder(ctx, orderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.ChatID != chatID) || err != nil {
		return
	}
	if o.QRISURL == "" || o.Status != "pending" {
		return
	}
	a.deleteQRIS(orderID)
	text, kb := a.qrisMessage(o, true)
	if id := a.sendQRISPhoto(chatID, o, text, kb); id != 0 {
		a.setUI(chatID, id)
	}
}

func (a *App) expireOrder(ctx context.Context, o *Order, msgID int) {
	if o.InvoiceID != "" {
		gwCtx, gwCancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := a.pay.CancelInvoice(gwCtx, o.InvoiceID); err != nil {
			log.Printf("cancel invoice %s di gateway saat expire: %v", o.InvoiceID, err)
		}
		gwCancel()
	}
	ok, err := a.store.CancelOrder(ctx, o.ID, o.ChatID)
	if err != nil {
		log.Printf("expire order %s: %v", o.ID, err)
		return
	}
	_ = a.store.SetInvoiceStatus(ctx, o.ID, "expired")
	a.deleteQRIS(o.ID)
	if !ok {
		return
	}
	log.Printf("order %s kedaluwarsa, stok dikembalikan", o.ID)
	if a.getUIOrder(o.ChatID) == o.ID {
		if msgID == 0 {
			msgID = a.getUI(o.ChatID)
		}
		a.clearUIOrder(o.ChatID)
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
				tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
			),
		)
		a.show(o.ChatID, msgID, a.message("msg_expired", map[string]any{"OrderID": o.ID}), &kb)
	}
}

func (a *App) startPoller(orderID string) {
	a.mu.Lock()
	if a.pollers[orderID] {
		a.mu.Unlock()
		return
	}
	a.pollers[orderID] = true
	a.mu.Unlock()
	go a.pollOrder(orderID)
}

func (a *App) pollOrder(orderID string) {
	defer func() {
		a.mu.Lock()
		delete(a.pollers, orderID)
		a.mu.Unlock()
	}()

	deadline := time.Now().Add(24 * time.Hour)
	interval := 5 * time.Second
	reviewNotified := false
	for time.Now().Before(deadline) {
		o, err := a.store.GetOrder(context.Background(), orderID)
		if err != nil || o.Status != "pending" || o.InvoiceID == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		inv, err := a.pay.InvoiceStatus(ctx, o.InvoiceID)
		cancel()
		if err != nil {
			log.Printf("poll invoice %s: %v", o.InvoiceID, err)
			interval = 15 * time.Second
			time.Sleep(interval)
			continue
		}
		switch paymentStatus(inv.Status) {
		case "paid":
			_ = a.store.SetInvoiceStatus(context.Background(), o.ID, "paid")
			a.deliverPaid(context.Background(), o.ChatID, 0, o.ID)
			return
		case "expired":
			a.expireOrder(context.Background(), o, 0)
			return
		case "review":
			if o.InvoiceStatus != "review" {
				_ = a.store.SetInvoiceStatus(context.Background(), o.ID, "review")
				if a.getUIOrder(o.ChatID) == o.ID {
					a.showReviewWindow(o.ChatID, 0, o)
				}
			}
			if !reviewNotified {
				reviewNotified = true
				a.notifyAdmins(fmt.Sprintf("⚠️ <b>Pembayaran REVIEW</b>\nOrder: <code>%s</code>\nInvoice: <code>%s</code>\nChat: <code>%d</code>\nTotal: %s\nPerlu rekonsiliasi manual di dashboard gateway. File dikirim otomatis setelah status paid.",
					o.ID, o.InvoiceID, o.ChatID, rupiah(o.GatewayTotal)))
			}
			interval = 30 * time.Second
		default:
			interval = 5 * time.Second
		}
		time.Sleep(interval)
	}
	log.Printf("poll order %s berhenti (timeout)", orderID)
}

func (a *App) resumePayments(ctx context.Context) {
	orders, err := a.store.ListPendingInvoices(ctx)
	if err != nil {
		log.Printf("resume payments: %v", err)
		return
	}
	for i := range orders {
		o := &orders[i]
		if o.InvoiceExpiresAt.Valid && o.InvoiceExpiresAt.Time.Before(time.Now().Add(-5*time.Minute)) && o.InvoiceStatus != "review" {
			a.expireOrder(ctx, o, 0)
			continue
		}
		log.Printf("resume poller order %s invoice %s", o.ID, o.InvoiceID)
		a.startPoller(o.ID)
	}
}

func (a *App) showStatusMenu(chatID int64, msgID int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.clearUIOrder(chatID)

	orders, err := a.store.ListOrders(ctx, chatID, 1)
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca pesanan.")
		return
	}
	if len(orders) == 0 {
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
				tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
			),
		)
		a.show(chatID, msgID, "📭 Kamu belum punya pesanan.", &kb)
		return
	}
	a.showOrderStatus(chatID, msgID, orders[0].ID)
}

func (a *App) showOrderStatus(chatID int64, msgID int, orderID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	o, err := a.store.GetOrder(ctx, orderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.ChatID != chatID) {
		a.showError(chatID, msgID, "❌ Pesanan tidak ditemukan.")
		return
	}
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca pesanan.")
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	switch o.Status {
	case "pending":
		a.setUIOrder(chatID, o.ID)
		if o.InvoiceID == "" {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔄 Buat QRIS", "payretry:"+o.ID),
				tgbotapi.NewInlineKeyboardButtonData("❌ Batalkan", "cancel:"+o.ID),
			))
		} else {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔄 Cek Status", "paycheck:"+o.ID),
				tgbotapi.NewInlineKeyboardButtonData("❌ Batalkan", "cancel:"+o.ID),
			))
		}
	case "paid", "processing":
		a.setUIOrder(chatID, o.ID)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📥 Ambil File", "deliver:"+o.ID),
		))
	default:
		a.clearUIOrder(chatID)
	}
	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📦 Pesanan Saya", "myorders"),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	a.show(chatID, msgID, orderCard(o, a.loc), &kb)
}

func (a *App) showMyOrders(chatID int64, msgID int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.clearUIOrder(chatID)

	orders, err := a.store.ListOrders(ctx, chatID, 10)
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca pesanan.")
		return
	}
	if len(orders) == 0 {
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
				tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
			),
		)
		a.show(chatID, msgID, "📭 Belum ada pesanan.", &kb)
		return
	}

	var b strings.Builder
	b.WriteString("📦 <b>Pesanan Saya</b>\n\n")
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, o := range orders {
		fmt.Fprintf(&b, "<code>%s</code>\n%s · %d akun · %s\n%s\n\n",
			o.ID, formatDateID(o.Date), o.Quantity, rupiah(o.TotalPrice), statusLabel(o.Status))
		row := []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("🔎 Cek", "status:"+o.ID),
		}
		if o.Status == "pending" {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel:"+o.ID))
		}
		if o.Status == "paid" || o.Status == "processing" {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("📥 Ambil", "deliver:"+o.ID))
		}
		rows = append(rows, row)
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
	))
	a.show(chatID, msgID, b.String(), &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) showCancelResult(chatID int64, msgID int, orderID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	o, err := a.store.GetOrder(ctx, orderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.ChatID != chatID) {
		a.showError(chatID, msgID, "❌ Pesanan tidak ditemukan.")
		return
	}
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membaca pesanan.")
		return
	}
	if o.Status != "pending" {
		a.showOrderStatus(chatID, msgID, orderID)
		return
	}

	if o.InvoiceID != "" {
		inv, err := a.pay.InvoiceStatus(ctx, o.InvoiceID)
		if err != nil {
			a.setUIOrder(chatID, o.ID)
			kb := a.qrisKeyboard(o.ID)
			a.show(chatID, msgID, "⚠️ Gagal mengecek status pembayaran. Pesanan belum dibatalkan, coba lagi.", &kb)
			return
		}
		switch paymentStatus(inv.Status) {
		case "paid":
			a.deliverPaid(ctx, chatID, msgID, o.ID)
			return
		case "review":
			a.showReviewWindow(chatID, msgID, o)
			return
		}
		gwCtx, gwCancel := context.WithTimeout(context.Background(), 20*time.Second)
		if cerr := a.pay.CancelInvoice(gwCtx, o.InvoiceID); cerr != nil {
			log.Printf("cancel invoice %s di gateway: %v", o.InvoiceID, cerr)
		}
		gwCancel()
	}

	ok, err := a.store.CancelOrder(ctx, orderID, chatID)
	if err != nil {
		a.showError(chatID, msgID, "❌ Gagal membatalkan pesanan.")
		return
	}
	if !ok {
		a.showOrderStatus(chatID, msgID, orderID)
		return
	}
	_ = a.store.SetInvoiceStatus(ctx, o.ID, "cancelled")
	a.deleteQRIS(o.ID)
	a.clearUIOrder(chatID)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛍️ Beli Akun", "shop"),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
	note := "✅ Pesanan dibatalkan. Stok dikembalikan."
	if o.InvoiceID != "" {
		note += "\n\n⚠️ Invoice QRIS sudah dibatalkan di gateway. Abaikan QR lama jika masih tampil di aplikasi."
		a.notifyAdmins(fmt.Sprintf("⚠️ <b>Order dibatalkan setelah invoice dibuat</b>\nOrder: <code>%s</code>\nInvoice: <code>%s</code>\nChat: <code>%d</code>\nInvoice sudah dibatalkan di gateway.",
			o.ID, o.InvoiceID, chatID))
	}
	a.show(chatID, msgID, note, &kb)
}

func (a *App) buildZip(ctx context.Context, accounts []Account, zipPath string) error {
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)

	for _, acc := range accounts {
		for _, key := range []string{acc.ObjectKey, acc.JSONKey} {
			obj, err := a.store.s3.GetObject(ctx, a.store.cfg.S3Bucket, key, minio.GetObjectOptions{})
			if err != nil {
				zw.Close()
				f.Close()
				return err
			}
			name := path.Join(acc.Name, path.Base(key))
			w, err := zw.Create(name)
			if err != nil {
				obj.Close()
				zw.Close()
				f.Close()
				return err
			}
			if _, err := io.Copy(w, obj); err != nil {
				obj.Close()
				zw.Close()
				f.Close()
				return err
			}
			obj.Close()
		}
	}

	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (a *App) show(chatID int64, msgID int, text string, kb *tgbotapi.InlineKeyboardMarkup) int {
	if msgID != 0 {
		e := tgbotapi.NewEditMessageText(chatID, msgID, text)
		e.ParseMode = tgbotapi.ModeHTML
		e.ReplyMarkup = kb
		if _, err := a.api.Send(e); err == nil {
			a.setUI(chatID, msgID)
			return msgID
		} else if strings.Contains(err.Error(), "message is not modified") {
			a.setUI(chatID, msgID)
			return msgID
		} else {
			log.Printf("edit pesan %d: %v", msgID, err)
		}
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	if kb != nil {
		msg.ReplyMarkup = kb
	}
	sent, err := a.api.Send(msg)
	if err != nil {
		log.Printf("kirim pesan ke %d: %v", chatID, err)
		return 0
	}
	a.setUI(chatID, sent.MessageID)
	return sent.MessageID
}

func (a *App) showError(chatID int64, msgID int, text string) int {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
	return a.show(chatID, msgID, text, &kb)
}

func emptyKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
}

func orderKeyboard(orderID string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔎 Cek Status", "status:"+orderID),
			tgbotapi.NewInlineKeyboardButtonData("📦 Pesanan Saya", "myorders"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Home", "home"),
		),
	)
}

func (a *App) notifyAdmins(text string) {
	for _, id := range a.cfg.AdminIDs {
		msg := tgbotapi.NewMessage(id, text)
		msg.ParseMode = tgbotapi.ModeHTML
		if _, err := a.api.Send(msg); err != nil {
			log.Printf("notif admin %d: %v", id, err)
		}
	}
}

func (a *App) deleteMessage(chatID int64, msgID int) {
	if msgID == 0 {
		return
	}
	if _, err := a.api.Request(tgbotapi.NewDeleteMessage(chatID, msgID)); err != nil {
		log.Printf("hapus pesan %d: %v", msgID, err)
	}
}

func (a *App) deleteUI(chatID int64) {
	a.mu.Lock()
	id := a.ui[chatID]
	delete(a.ui, chatID)
	a.mu.Unlock()
	if id != 0 {
		a.deleteMessage(chatID, id)
	}
}

func (a *App) setUIOrder(chatID int64, orderID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.uiOrder[chatID] = orderID
}

func (a *App) getUIOrder(chatID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uiOrder[chatID]
}

func (a *App) clearUIOrder(chatID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.uiOrder, chatID)
}

func (a *App) setQRIS(chatID int64, orderID string, msgID int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.qris[orderID] = qrisRef{chatID: chatID, msgID: msgID}
}

func (a *App) getQRIS(orderID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.qris[orderID].msgID
}

func (a *App) deleteQRIS(orderID string) {
	a.mu.Lock()
	ref := a.qris[orderID]
	delete(a.qris, orderID)
	a.mu.Unlock()
	if ref.msgID != 0 {
		a.deleteMessage(ref.chatID, ref.msgID)
	}
}

func (a *App) setUI(chatID int64, msgID int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ui[chatID] = msgID
	a.uiVer[chatID]++
}

func (a *App) uiVersion(chatID int64) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uiVer[chatID]
}

func (a *App) getUI(chatID int64) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ui[chatID]
}

func (a *App) setSession(chatID int64, s *session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[chatID] = s
}

func (a *App) getSession(chatID int64) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[chatID]
	if s == nil {
		return nil
	}
	if time.Now().After(s.expires) {
		delete(a.sessions, chatID)
		return nil
	}
	return s
}

func (a *App) clearSession(chatID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, chatID)
}

func (a *App) tryBusy(chatID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy[chatID] {
		return false
	}
	a.busy[chatID] = true
	return true
}

func (a *App) clearBusy(chatID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.busy, chatID)
}

func newToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

func orderCard(o *Order, loc *time.Location) string {
	text := fmt.Sprintf(`🧾 <b>Order %s</b>

Tanggal: <b>%s</b>
Jumlah: <b>%d akun</b>
Harga: %s/akun
Total akun: <b>%s</b>
Status: <b>%s</b>`,
		o.ID, formatDateID(o.Date), o.Quantity, rupiah(o.PricePerAccount), rupiah(o.TotalPrice), statusLabel(o.Status))
	if o.InvoiceID != "" {
		text += fmt.Sprintf("\nTotal bayar: <b>%s</b>\nPembayaran: %s", rupiah(o.GatewayTotal), invoiceStatusLabel(o.InvoiceStatus))
		if o.InvoiceExpiresAt.Valid && o.Status == "pending" {
			text += "\nKedaluwarsa: " + o.InvoiceExpiresAt.Time.In(loc).Format("02 Jan 2006 15:04")
		}
	}
	text += "\nDibuat: " + o.CreatedAt.In(loc).Format("02 Jan 2006 15:04")
	return text
}

func invoiceStatusLabel(status string) string {
	switch status {
	case "paid":
		return "✅ Lunas"
	case "expired":
		return "⌛ Kedaluwarsa"
	case "review":
		return "⚠️ Menunggu review"
	case "cancelled":
		return "❌ Dibatalkan"
	default:
		return "⏳ Menunggu pembayaran"
	}
}

func feeEstimate(amount int64) int64 {
	if amount <= 0 {
		return 0
	}
	return ((amount + 9999) / 10000) * 110
}

func statusLabel(status string) string {
	switch status {
	case "pending":
		return "⏳ Menunggu pembayaran"
	case "paid":
		return "✅ Lunas, sedang diproses"
	case "sent":
		return "📦 Selesai — file terkirim"
	case "review":
		return "⚠️ Menunggu review"
	case "cancelled":
		return "❌ Dibatalkan"
	default:
		return status
	}
}

func rupiah(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteByte(s[i])
	}
	out := "Rp " + b.String()
	if neg {
		out = "-" + out
	}
	return out
}

func formatDateID(t time.Time) string {
	months := [...]string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}
	return fmt.Sprintf("%02d %s %d", t.Day(), months[int(t.Month())-1], t.Year())
}

func formatDateFullID(t time.Time) string {
	months := [...]string{"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}
	return fmt.Sprintf("%02d %s", t.Day(), months[int(t.Month())-1])
}

func accountAgeDays(t time.Time, loc *time.Location) int {
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	age := int(today.Sub(d).Hours() / 24)
	if age < 0 {
		age = 0
	}
	return age
}

func formatDateString(value string, loc *time.Location) string {
	t, err := time.ParseInLocation("2006-01-02", value, loc)
	if err != nil {
		return value
	}
	return formatDateID(t)
}
