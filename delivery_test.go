package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestDeliveryFlow(t *testing.T) {
	dsn := os.Getenv("TEST_DSN")
	if dsn == "" {
		t.Skip("TEST_DSN tidak diisi")
	}
	loc, _ := time.LoadLocation("Asia/Jakarta")
	cfg := Config{MySQLDSN: dsn, S3Endpoint: "http://127.0.0.1:9001", S3AccessKey: "x", S3SecretKey: "x", S3Bucket: "tiktok-account", S3Region: "us-east-1"}
	st, err := NewStore(cfg, loc)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	dates, _ := st.Dates(ctx)
	if len(dates) == 0 {
		t.Skip("stok kosong")
	}
	date := dates[0].Date.Format("2006-01-02")
	base := dates[0].Count
	ts := time.Now().UnixNano()
	id := fmt.Sprintf("TEST-del-%d", ts)
	key := fmt.Sprintf("test-del-%d", ts)
	defer func() {
		_ = st.Release(ctx, id)
		_, _ = st.db.ExecContext(ctx, `DELETE FROM orders WHERE id = ?`, id)
		if n, _ := st.Available(ctx, date); n != base {
			t.Errorf("stok akhir %d, expected %d", n, base)
		}
	}()

	if _, _, dup, err := st.Reserve(ctx, id, key, 999, "tester", date, 1, 0, 1000); err != nil || dup != nil {
		t.Fatalf("reserve: dup=%v err=%v", dup, err)
	}
	if n, _ := st.Available(ctx, date); n != base-1 {
		t.Fatalf("stok setelah reserve %d", n)
	}
	ok, err := st.ClaimDelivery(ctx, id, 999)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if ok2, _ := st.ClaimDelivery(ctx, id, 999); ok2 {
		t.Fatal("claim kedua harus false")
	}
	accs, err := st.OrderAccounts(ctx, id)
	if err != nil || len(accs) != 1 {
		t.Fatalf("accounts: %d err=%v", len(accs), err)
	}
	if err := st.Finalize(ctx, id); err != nil {
		t.Fatal(err)
	}
	o, _ := st.GetOrder(ctx, id)
	if o == nil || o.Status != "sent" {
		t.Fatalf("status setelah finalize: %v", o)
	}
	if err := st.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.Available(ctx, date); n != base {
		t.Fatalf("stok setelah release %d, expected %d", n, base)
	}
	t.Log("delivery flow OK: reserve -> claim -> accounts -> finalize -> release")
}
