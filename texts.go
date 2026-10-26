package main

import (
	"log"
	"strings"
	"text/template"
	"time"
)

type textView struct {
	Key          string
	Label        string
	Description  string
	Placeholders string
	Value        string
}

type textDef struct {
	Key          string
	Label        string
	Description  string
	Placeholders string
	Default      string
}

// Urutan di sini menentukan urutan tampil di halaman admin.
var textDefs = []textDef{
	{
		Key:          "msg_home",
		Label:        "Menu utama",
		Description:  "Pesan yang muncul saat pelanggan membuka bot atau menekan Home.",
		Placeholders: "{{.Star}} {{.Stock}} {{.Price}} {{.BotName}}",
		Default: `{{.Star}} <b>Menu Utama</b>

Harga: <b>{{.Price}}/akun</b>

Silakan pilih menu:`,
	},
	{
		Key:          "msg_help",
		Label:        "Bantuan (/help)",
		Description:  "Teks bantuan yang muncul saat pelanggan mengetik /help.",
		Placeholders: "{{.Price}} {{.BotName}}",
		Default:      helpText,
	},
	{
		Key:          "msg_shop",
		Label:        "Header Beli Akun",
		Description:  "Judul saat pelanggan membuka daftar tanggal akun.",
		Placeholders: "{{.Price}} {{.BotName}}",
		Default: `🛍️ <b>Beli Akun</b>

Pilih tanggal akun:`,
	},
	{
		Key:          "msg_date",
		Label:        "Pilih jumlah (preset)",
		Description:  "Teks saat pelanggan memilih salah satu tanggal akun.",
		Placeholders: "{{.Date}} {{.Stock}} {{.Price}}",
		Default: `📅 <b>{{.Date}}</b>
Harga: <b>{{.Price}}/akun</b>

Pilih jumlah akun:`,
	},
	{
		Key:          "msg_custom",
		Label:        "Input jumlah custom",
		Description:  "Teks saat pelanggan memilih input jumlah manual.",
		Placeholders: "{{.Date}} {{.Stock}} {{.Max}}",
		Default: `✏️ <b>Custom jumlah</b>
Tanggal: <b>{{.Date}}</b>

Ketik jumlah akun (1–{{.Max}}):`,
	},
	{
		Key:          "msg_confirm",
		Label:        "Konfirmasi pesanan",
		Description:  "Ringkasan pesanan sebelum pelanggan menekan Bayar.",
		Placeholders: "{{.Date}} {{.Qty}} {{.Price}} {{.Total}} {{.Fee}} {{.GrandTotal}}",
		Default: `🛒 <b>Konfirmasi Pesanan</b>

Tanggal: <b>{{.Date}}</b>
Jumlah: <b>{{.Qty}} akun</b>
Harga: <b>{{.Price}}/akun</b>
Total akun: <b>{{.Total}}</b>
Biaya gateway (est.): <b>{{.Fee}}</b>
Total bayar: <b>{{.GrandTotal}}</b>

Stok direservasi saat kamu klik <b>Bayar</b>. File akun dikirim otomatis setelah pembayaran terverifikasi.`,
	},
	{
		Key:          "msg_qris",
		Label:        "Instruksi pembayaran QRIS",
		Description:  "Caption foto QRIS (teks dan gambar dikirim dalam satu bubble).",
		Placeholders: "{{.Head}} {{.OrderID}} {{.Date}} {{.Qty}} {{.Price}} {{.Fee}} {{.Total}} {{.Expires}}",
		Default: `{{.Head}}

Order: <code>{{.OrderID}}</code>
Tanggal: <b>{{.Date}}</b>
Jumlah: <b>{{.Qty}} akun</b>
Harga akun: <b>{{.Price}}</b>
Biaya gateway: <b>{{.Fee}}</b>
Total bayar: <b>{{.Total}}</b>
Status: ⏳ Menunggu pembayaran
Kedaluwarsa: <b>{{.Expires}}</b>

Scan QRIS yang dikirim, lalu tunggu. File akun dikirim otomatis setelah pembayaran terverifikasi.`,
	},
	{
		Key:          "msg_review",
		Label:        "Pembayaran review",
		Description:  "Pesan saat pembayaran masuk status review di gateway.",
		Placeholders: "{{.OrderID}} {{.Invoice}} {{.Qty}} {{.Total}}",
		Default: `⏳ <b>Pembayaran diterima — menunggu review</b>

Order: <code>{{.OrderID}}</code>
Invoice: <code>{{.Invoice}}</code>
Jumlah: <b>{{.Qty}} akun</b>
Total bayar: <b>{{.Total}}</b>

Provider menandai pembayaran ini <b>review</b> (perlu rekonsiliasi manual).
<b>Jangan bayar ulang.</b> File akun akan dikirim otomatis setelah status menjadi <b>paid</b>.`,
	},
	{
		Key:          "msg_delivery",
		Label:        "Caption file terkirim",
		Description:  "Caption saat file ZIP akun dikirim ke pelanggan.",
		Placeholders: "{{.OrderID}} {{.Date}} {{.Qty}} {{.Total}}",
		Default: `📦 <b>Order {{.OrderID}} — LUNAS</b>
Tanggal: <b>{{.Date}}</b>
Jumlah: <b>{{.Qty}} akun</b>
Total bayar: <b>{{.Total}}</b>

Simpan file ini.`,
	},
	{
		Key:          "msg_expired",
		Label:        "Pesanan kedaluwarsa",
		Description:  "Pesan saat QRIS kedaluwarsa dan stok dikembalikan.",
		Placeholders: "{{.OrderID}}",
		Default:      `⌛ QRIS order <code>{{.OrderID}}</code> kedaluwarsa. Stok sudah dikembalikan.`,
	},
}

var defaultTexts = func() map[string]string {
	out := make(map[string]string, len(textDefs))
	for _, d := range textDefs {
		out[d.Key] = d.Default
	}
	return out
}()

func (a *App) loadTexts(values map[string]string) {
	a.textsMu.Lock()
	defer a.textsMu.Unlock()
	if a.texts == nil {
		a.texts = make(map[string]string, len(textDefs))
	}
	for _, d := range textDefs {
		if v := strings.TrimSpace(values[d.Key]); v != "" {
			a.texts[d.Key] = v
		} else {
			a.texts[d.Key] = d.Default
		}
	}
}

func (a *App) applyTextValues(values map[string]string) {
	a.textsMu.Lock()
	defer a.textsMu.Unlock()
	if a.texts == nil {
		a.texts = make(map[string]string, len(textDefs))
	}
	for _, d := range textDefs {
		v := strings.TrimSpace(values[d.Key])
		if v == "" {
			v = d.Default
		}
		a.texts[d.Key] = v
	}
}

// message merender template teks bot. Bila template dari admin rusak,
// dipakai teks default agar bot tetap berjalan.
func (a *App) message(key string, data any) string {
	if m, ok := data.(map[string]any); ok {
		if _, exists := m["BotName"]; !exists {
			m["BotName"] = a.botName()
		}
	}
	a.textsMu.RLock()
	tmpl := a.texts[key]
	a.textsMu.RUnlock()
	if tmpl == "" {
		tmpl = defaultTexts[key]
	}
	if out, err := renderBotText(tmpl, data); err == nil && out != "" {
		return out
	} else if err != nil {
		log.Printf("teks bot %s tidak valid: %v", key, err)
	}
	if out, err := renderBotText(defaultTexts[key], data); err == nil && out != "" {
		return out
	}
	return defaultTexts[key]
}

func validateBotText(tmpl string) error {
	_, err := template.New("msg").Parse(tmpl)
	return err
}

func renderBotText(tmpl string, data any) (string, error) {
	t, err := template.New("msg").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (a *App) botName() string {
	if a.api != nil && a.api.Self.UserName != "" {
		return a.api.Self.UserName
	}
	return ""
}

func botTextData(extra map[string]any) map[string]any {
	now := time.Now()
	out := map[string]any{
		"BotName": "-",
		"Now":     now,
		"Year":    now.Year(),
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
