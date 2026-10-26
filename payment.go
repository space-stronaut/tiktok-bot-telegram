package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const paymentExpiryMinutes = 15

type PanzzPay struct {
	base    string
	key     string
	webhook string
	hc      *http.Client
}

// Invoice adalah representasi invoice yang dipakai bot dan store.
// Field-nya dipetakan dari respons PanzzPay agar pemakaian lama tetap kompatibel.
type Invoice struct {
	Success   bool
	InvoiceID string
	Amount    int64
	Fee       int64
	Total     int64
	QRISImage string
	Status    string
	ExpiredAt string
	CreatedAt string
}

type panzzPayment struct {
	ID              string `json:"id"`
	TransactionID   string `json:"transaction_id"`
	Amount          int64  `json:"amount"`
	UniqueAmount    int64  `json:"unique_amount"`
	Total           int64  `json:"total"`
	Status          string `json:"status"`
	QRISPayload     string `json:"qris_payload"`
	QRISDataURL     string `json:"qr_data_url"`
	QRISPNGDataURL  string `json:"qr_png_data_url"`
	CreatedAt       int64  `json:"created_at"`
	ExpiresAt       int64  `json:"expires_at"`
	CustomerOrderID string `json:"customer_order_id"`
}

func (r panzzPayment) invoice() *Invoice {
	inv := &Invoice{
		Success:   true,
		InvoiceID: firstNonEmpty(r.TransactionID, r.ID, r.CustomerOrderID),
		Amount:    r.Amount,
		Status:    r.Status,
		QRISImage: firstNonEmpty(r.QRISPNGDataURL, r.QRISDataURL),
	}
	total := r.Total
	if total == 0 {
		total = r.UniqueAmount
	}
	if total < r.Amount {
		total = r.Amount
	}
	inv.Total = total
	inv.Fee = total - r.Amount
	if r.ExpiresAt > 0 {
		inv.ExpiredAt = time.UnixMilli(r.ExpiresAt).UTC().Format(time.RFC3339)
	}
	if r.CreatedAt > 0 {
		inv.CreatedAt = time.UnixMilli(r.CreatedAt).UTC().Format(time.RFC3339)
	}
	return inv
}

func NewPanzzPay(baseURL, apiKey, webhookURL string) *PanzzPay {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &PanzzPay{
		base:    baseURL,
		key:     strings.TrimSpace(apiKey),
		webhook: strings.TrimSpace(webhookURL),
		hc:      &http.Client{Timeout: 25 * time.Second},
	}
}

func (p *PanzzPay) CreateInvoice(ctx context.Context, amount int64, customerOrderID string) (*Invoice, error) {
	if amount < 1000 {
		return nil, fmt.Errorf("nominal minimal Rp 1.000")
	}
	if strings.TrimSpace(customerOrderID) == "" {
		return nil, fmt.Errorf("customer_order_id kosong")
	}
	body, err := json.Marshal(map[string]any{
		"amount":            amount,
		"customer_order_id": customerOrderID,
		"webhook_url":       p.webhook,
		"expiry_minutes":    paymentExpiryMinutes,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/api/payment", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", p.key)
	req.Header.Set("Content-Type", "application/json")

	var raw panzzPayment
	if err := p.do(req, &raw); err != nil {
		return nil, err
	}
	inv := raw.invoice()
	if inv.InvoiceID == "" {
		return nil, fmt.Errorf("gateway tidak mengembalikan transaction_id")
	}
	if inv.QRISImage == "" {
		return nil, fmt.Errorf("gateway tidak mengembalikan gambar QRIS")
	}
	return inv, nil
}

func (p *PanzzPay) InvoiceStatus(ctx context.Context, transactionID string) (*Invoice, error) {
	if strings.TrimSpace(transactionID) == "" {
		return nil, fmt.Errorf("transaction_id kosong")
	}
	q := url.Values{"transaction_id": {transactionID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/api/payment?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", p.key)
	var raw panzzPayment
	if err := p.do(req, &raw); err != nil {
		return nil, err
	}
	inv := raw.invoice()
	if inv.InvoiceID == "" {
		inv.InvoiceID = transactionID
	}
	return inv, nil
}

func (p *PanzzPay) CancelInvoice(ctx context.Context, transactionID string) error {
	if strings.TrimSpace(transactionID) == "" {
		return fmt.Errorf("transaction_id kosong")
	}
	body, err := json.Marshal(map[string]string{"transaction_id": transactionID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/api/payment/cancel", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", p.key)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Success bool   `json:"success"`
		OK      bool   `json:"ok"`
		Status  string `json:"status"`
	}
	if err := p.do(req, &out); err != nil {
		return err
	}
	if !out.Success && !out.OK {
		return fmt.Errorf("gateway menolak pembatalan (status=%s)", out.Status)
	}
	return nil
}

func (p *PanzzPay) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/api/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", p.key)
	var out struct {
		Status string `json:"status"`
	}
	if err := p.do(req, &out); err != nil {
		return err
	}
	if out.Status != "" && out.Status != "healthy" {
		return fmt.Errorf("gateway %s", out.Status)
	}
	return nil
}

func (p *PanzzPay) do(req *http.Request, out any) error {
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gateway %s: HTTP %d: %s", req.URL.Path, resp.StatusCode, snippet(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("respons gateway tidak valid: %w (%s)", err, snippet(raw))
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func parseGatewayTime(value string, loc *time.Location) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

func paymentStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "paid", "success", "settlement", "settled":
		return "paid"
	case "expired", "expire", "failed", "failure", "cancelled", "canceled", "timeout":
		return "expired"
	case "review", "pending_review":
		return "review"
	default:
		return "pending"
	}
}
