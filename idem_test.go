package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestIdempotency(t *testing.T) {
	dsn := os.Getenv("TEST_DSN")
	if dsn == "" {
		t.Skip("TEST_DSN tidak diisi")
	}
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		MySQLDSN:    dsn,
		S3Endpoint:  "http://127.0.0.1:9001",
		S3AccessKey: "x",
		S3SecretKey: "x",
		S3Bucket:    "tiktok-account",
		S3Region:    "us-east-1",
	}
	st, err := NewStore(cfg, loc)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	dates, err := st.Dates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var date string
	var base int
	for _, d := range dates {
		if d.Count >= 4 {
			date = d.Date.Format("2006-01-02")
			base = d.Count
			break
		}
	}
	if date == "" {
		t.Skip("tidak ada tanggal dengan >= 4 stok")
	}
	t.Logf("tanggal uji %s, stok awal %d", date, base)

	defer func() {
		rows, err := st.db.QueryContext(ctx, `SELECT id FROM orders WHERE id LIKE 'TEST-%'`)
		if err == nil {
			var ids []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				_ = st.Release(ctx, id)
			}
		}
		db, _ := sql.Open("mysql", dsn)
		_, _ = db.ExecContext(ctx, `DELETE FROM orders WHERE id LIKE 'TEST-%'`)
		db.Close()
		if n, err := st.Available(ctx, date); err == nil && n != base {
			t.Errorf("stok akhir %d, expected %d", n, base)
		}
	}()

	ts := time.Now().UnixNano()
	key := fmt.Sprintf("test-same-%d", ts)

	type res struct {
		dup *Order
		err error
	}
	var wg sync.WaitGroup
	out := make(chan res, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("TEST-same-%d-%d", ts, i)
			_, _, dup, err := st.Reserve(ctx, id, key, 999, "", date, 1, 0, 1000)
			out <- res{dup, err}
		}(i)
	}
	wg.Wait()
	close(out)

	fresh, duplicate := 0, 0
	for r := range out {
		if r.err != nil {
			t.Fatalf("reserve same-key: %v", r.err)
		}
		if r.dup != nil {
			duplicate++
		} else {
			fresh++
		}
	}
	if fresh != 1 || duplicate != 1 {
		t.Fatalf("same-key: fresh=%d duplicate=%d, expected 1/1", fresh, duplicate)
	}

	var orderID string
	if err := st.db.QueryRowContext(ctx, `SELECT id FROM orders WHERE idempotency_key = ?`, key).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.Available(ctx, date); n != base-1 {
		t.Fatalf("stok setelah same-key %d, expected %d", n, base-1)
	}

	_, _, again, err := st.Reserve(ctx, fmt.Sprintf("TEST-seq-%d", ts), key, 999, "", date, 1, 0, 1000)
	if err != nil || again == nil || again.ID != orderID {
		t.Fatalf("sequential duplicate: dup=%v err=%v", again, err)
	}
	if n, _ := st.Available(ctx, date); n != base-1 {
		t.Fatalf("stok setelah duplicate %d, expected %d", n, base-1)
	}

	_ = st.Release(ctx, orderID)
	if n, _ := st.Available(ctx, date); n != base {
		t.Fatalf("stok setelah release %d, expected %d", n, base)
	}

	raceQty := base/2 + 1
	if raceQty < 2 {
		raceQty = 2
	}
	keyA := fmt.Sprintf("test-race-a-%d", ts)
	keyB := fmt.Sprintf("test-race-b-%d", ts)
	out2 := make(chan res, 2)
	for i, k := range []string{keyA, keyB} {
		wg.Add(1)
		go func(i int, k string) {
			defer wg.Done()
			id := fmt.Sprintf("TEST-race-%d-%d", ts, i)
			_, _, dup, err := st.Reserve(ctx, id, k, 999, "", date, raceQty, 0, 1000)
			out2 <- res{dup, err}
		}(i, k)
	}
	wg.Wait()
	close(out2)

	ok, insufficient := 0, 0
	for r := range out2 {
		switch {
		case r.err == nil && r.dup == nil:
			ok++
		case errors.Is(r.err, ErrInsufficient):
			insufficient++
		default:
			t.Fatalf("race: %v", r.err)
		}
	}
	if ok != 1 || insufficient != 1 {
		t.Fatalf("race: ok=%d insufficient=%d, expected 1/1", ok, insufficient)
	}
	t.Logf("idempotency OK: one order for same key, race rejected properly")
}
