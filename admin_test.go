package main

import (
	"bytes"
	"database/sql"
	"testing"
	"time"
)

func TestAdminTemplatesRender(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewAdminServer(&App{loc: loc})
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	now := time.Now()
	order := &Order{
		ID:              "ORD-TEST",
		ChatID:          123,
		Username:        "tester",
		Date:            now,
		Quantity:        2,
		AmountBytes:     1234567,
		PricePerAccount: 1000,
		TotalPrice:      2000,
		Status:          "pending",
		InvoiceID:       "INV123",
		InvoiceStatus:   "review",
		PaymentFee:      110,
		GatewayTotal:    2110,
		QRISURL:         "https://example.com/qris.png",
		InvoiceExpiresAt: sql.NullTime{
			Time:  now.Add(15 * time.Minute),
			Valid: true,
		},
		CreatedAt: now,
		PaidAt:    sql.NullTime{Time: now, Valid: true},
		SentAt:    sql.NullTime{Time: now, Valid: true},
	}
	data := &pageData{
		Title:     "Test",
		User:      "admin",
		BotName:   "testbot",
		CSRF:      "csrf",
		Flash:     "ok",
		FlashKind: "success",
		Stats:     &AdminStats{TotalOrders: 1, Review: 1, TotalAccounts: 10, Available: 9},
		Orders:    []Order{*order},
		Order:     order,
		Accounts:  []Account{{ID: 1, Name: "acc1", Batch: "b1", Size: 1000, ObjectKey: "k"}},
		Stock:     []AdminStockRow{{Date: now, Total: 10, Available: 9, Reserved: 1}},
		Settings:  &settingsView{BotTokenSet: true, BotTokenMasked: "â€¢â€¢â€¢â€¢abcd", AdminIDs: "1", PricePerAccount: 1000, MaxAccounts: 20, MaxZipMB: 45, BotName: "testbot"},
		Texts: []textView{
			{Key: "msg_home", Label: "Menu utama", Description: "test", Placeholders: "{{.Stock}}", Value: "Halo {{.BotName}}"},
		},
		Page:         1,
		TotalPages:   1,
		Total:        1,
		PrevURL:      "/orders?page=1",
		NextURL:      "/orders?page=1",
		DefaultPrice: 1000,
		MaxAccounts:  20,
	}

	for name, tmpl := range s.pages {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "layout.html", data); err != nil {
			t.Errorf("render %s: %v", name, err)
		}
		if buf.Len() == 0 {
			t.Errorf("render %s: output kosong", name)
		}
	}

	var login bytes.Buffer
	if err := s.loginTmpl.ExecuteTemplate(&login, "login.html", &pageData{Err: "salah"}); err != nil {
		t.Errorf("render login: %v", err)
	}
}
