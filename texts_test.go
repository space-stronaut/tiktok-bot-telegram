package main

import (
	"strings"
	"testing"
)

func TestBotTextDefaultsRender(t *testing.T) {
	data := botTextData(map[string]any{
		"Stock":      10,
		"Price":      "Rp 1.000",
		"Date":       "16 Okt 2026",
		"Max":        25,
		"Qty":        2,
		"Total":      "Rp 2.000",
		"Fee":        "Rp 110",
		"GrandTotal": "Rp 2.110",
		"Head":       "💳 Pembayaran QRIS",
		"OrderID":    "ORD-TEST",
		"Invoice":    "INV-TEST",
		"Expires":    "16 Okt 2026 15:00",
	})
	for _, d := range textDefs {
		if err := validateBotText(d.Default); err != nil {
			t.Fatalf("%s: template default tidak valid: %v", d.Key, err)
		}
		out, err := renderBotText(d.Default, data)
		if err != nil {
			t.Fatalf("%s: render gagal: %v", d.Key, err)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatalf("%s: hasil render kosong", d.Key)
		}
	}
}

func TestBotMessageFallsBackOnBrokenTemplate(t *testing.T) {
	app := &App{texts: map[string]string{"msg_home": "Halo {{.Stock"}}
	out := app.message("msg_home", map[string]any{"Stock": 3})
	if !strings.Contains(out, "Menu Utama") {
		t.Fatalf("harus fallback ke default, dapat: %q", out)
	}
}
