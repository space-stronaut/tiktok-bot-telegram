package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	ErrInsufficient = errors.New("stok tidak cukup")
	ErrTooLarge     = errors.New("ukuran pesanan terlalu besar")
)

const schemaAccounts = `CREATE TABLE IF NOT EXISTS accounts (
	id BIGINT AUTO_INCREMENT PRIMARY KEY,
	object_key VARCHAR(512) NOT NULL,
	json_key VARCHAR(512) NOT NULL,
	account_name VARCHAR(128) NOT NULL,
	batch_prefix VARCHAR(128) NOT NULL,
	account_date DATE NOT NULL,
	size_bytes BIGINT NOT NULL DEFAULT 0,
	status VARCHAR(16) NOT NULL DEFAULT 'available',
	order_id VARCHAR(80) NULL,
	sold_to BIGINT NULL,
	sold_at DATETIME NULL,
	reserved_at DATETIME NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
	UNIQUE KEY uniq_object_key (object_key),
	KEY idx_date_status (account_date, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

const schemaOrders = `CREATE TABLE IF NOT EXISTS orders (
	id VARCHAR(80) PRIMARY KEY,
	chat_id BIGINT NOT NULL,
	username VARCHAR(255) NOT NULL DEFAULT '',
	account_date DATE NOT NULL,
	quantity INT NOT NULL,
	amount_bytes BIGINT NOT NULL DEFAULT 0,
	price_per_account BIGINT NOT NULL DEFAULT 0,
	total_price BIGINT NOT NULL DEFAULT 0,
	status VARCHAR(16) NOT NULL DEFAULT 'pending',
	invoice_id VARCHAR(128) NULL,
	invoice_status VARCHAR(32) NOT NULL DEFAULT '',
	payment_fee BIGINT NOT NULL DEFAULT 0,
	gateway_total BIGINT NOT NULL DEFAULT 0,
	qris_url TEXT NULL,
	invoice_expires_at DATETIME NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	paid_at DATETIME NULL,
	sent_at DATETIME NULL,
	idempotency_key VARCHAR(96) NULL,
	KEY idx_chat_created (chat_id, created_at),
	KEY idx_status_created (status, created_at),
	UNIQUE KEY uniq_idempotency (idempotency_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

var orderMigrations = []string{
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS price_per_account BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS total_price BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS paid_at DATETIME NULL`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(96) NULL`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS invoice_id VARCHAR(128) NULL`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS invoice_status VARCHAR(32) NOT NULL DEFAULT ''`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS payment_fee BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS gateway_total BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS qris_url TEXT NULL`,
	`ALTER TABLE orders ADD COLUMN IF NOT EXISTS invoice_expires_at DATETIME NULL`,
	`ALTER TABLE orders MODIFY status VARCHAR(16) NOT NULL DEFAULT 'pending'`,
}

type Account struct {
	ID        int64
	ObjectKey string
	JSONKey   string
	Name      string
	Batch     string
	Size      int64
}

type DateStock struct {
	Date  time.Time
	Count int
}

type Order struct {
	ID               string
	ChatID           int64
	Username         string
	Date             time.Time
	Quantity         int
	AmountBytes      int64
	PricePerAccount  int64
	TotalPrice       int64
	Status           string
	InvoiceID        string
	InvoiceStatus    string
	PaymentFee       int64
	GatewayTotal     int64
	QRISURL          string
	InvoiceExpiresAt sql.NullTime
	CreatedAt        time.Time
	PaidAt           sql.NullTime
	SentAt           sql.NullTime
}

type Store struct {
	cfg Config
	db  *sql.DB
	s3  *minio.Client
	loc *time.Location
}

func NewStore(cfg Config, loc *time.Location) (*Store, error) {
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaAccounts); err != nil {
		db.Close()
		return nil, fmt.Errorf("buat tabel accounts: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaOrders); err != nil {
		db.Close()
		return nil, fmt.Errorf("buat tabel orders: %w", err)
	}
	for _, m := range orderMigrations {
		if _, err := db.ExecContext(ctx, m); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrasi orders: %w", err)
		}
	}
	var idxCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'orders' AND INDEX_NAME = 'uniq_idempotency'`).Scan(&idxCount); err != nil {
		db.Close()
		return nil, fmt.Errorf("cek index idempotency: %w", err)
	}
	if idxCount == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE orders ADD UNIQUE KEY uniq_idempotency (idempotency_key)`); err != nil {
			db.Close()
			return nil, fmt.Errorf("buat index idempotency: %w", err)
		}
	}
	if err := ensureAdminSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	endpoint := strings.TrimSpace(cfg.S3Endpoint)
	secure := strings.HasPrefix(strings.ToLower(endpoint), "https://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimSuffix(endpoint, "/")

	s3c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: secure,
		Region: cfg.S3Region,
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("inisialisasi s3: %w", err)
	}

	return &Store{cfg: cfg, db: db, s3: s3c, loc: loc}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

type SyncResult struct {
	Total      int
	Remapped   int
	Pruned     int
	Duplicates int
	Unresolved int
}

type s3Account struct {
	Key     string
	JSONKey string
	Name    string
	Batch   string
	Date    string
	Size    int64
}

func (s *Store) Sync(ctx context.Context) (int, error) {
	res, err := s.sync(ctx, false)
	return res.Total, err
}

// SyncReconcile menyamakan stok MySQL dengan object storage, termasuk
// membersihkan baris lama yang objeknya sudah tidak ada dan memetakan ulang
// akun terjual/reserved yang objeknya berpindah path.
func (s *Store) SyncReconcile(ctx context.Context) (SyncResult, error) {
	return s.sync(ctx, true)
}

func (s *Store) sync(ctx context.Context, reconcile bool) (SyncResult, error) {
	var res SyncResult
	recs := make(map[string]s3Account)
	byBase := make(map[string][]string)

	for obj := range s.s3.ListObjects(ctx, s.cfg.S3Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return res, obj.Err
		}
		if !strings.HasSuffix(obj.Key, ".tar.gz") {
			continue
		}
		parts := strings.Split(obj.Key, "/")
		if len(parts) < 3 {
			continue
		}
		batch := parts[0]
		name := parts[len(parts)-2]
		date := parseAccountDate(batch, s.loc)
		if date.IsZero() {
			date = parseAccountDate(name, s.loc)
		}
		if date.IsZero() {
			date = obj.LastModified.In(s.loc)
		}
		rec := s3Account{
			Key:     obj.Key,
			JSONKey: strings.TrimSuffix(obj.Key, ".tar.gz") + ".json",
			Name:    name,
			Batch:   batch,
			Date:    date.Format("2006-01-02"),
			Size:    obj.Size,
		}
		recs[obj.Key] = rec
		base := path.Base(obj.Key)
		byBase[base] = append(byBase[base], obj.Key)
	}
	res.Total = len(recs)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	// Akun yang sudah terjual atau sedang direservasi tidak boleh muncul lagi
	// sebagai stok available meskipun file fisiknya masih ada di storage.
	lockedBase, err := soldReservedBases(ctx, tx)
	if err != nil {
		return res, err
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO accounts
		(object_key, json_key, account_name, batch_prefix, account_date, size_bytes)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			json_key = VALUES(json_key),
			account_name = VALUES(account_name),
			batch_prefix = VALUES(batch_prefix),
			account_date = VALUES(account_date),
			size_bytes = VALUES(size_bytes)`)
	if err != nil {
		return res, err
	}
	defer stmt.Close()

	for key, rec := range recs {
		if lockedBase[path.Base(key)] {
			continue
		}
		if _, err := stmt.ExecContext(ctx,
			rec.Key, rec.JSONKey, rec.Name, rec.Batch, rec.Date, rec.Size,
		); err != nil {
			return res, err
		}
	}

	if reconcile {
		if err := reconcileAccounts(ctx, tx, recs, byBase, &res); err != nil {
			return SyncResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return SyncResult{}, err
	}
	if reconcile {
		log.Printf("rekonsiliasi stok: %d objek S3, remap %d, hapus stale %d, duplikat %d, unresolved %d",
			res.Total, res.Remapped, res.Pruned, res.Duplicates, res.Unresolved)
	}
	return res, nil
}

func soldReservedBases(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	out := make(map[string]bool)
	rows, err := tx.QueryContext(ctx, `SELECT object_key FROM accounts WHERE status IN ('sold', 'reserved')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out[path.Base(key)] = true
	}
	return out, rows.Err()
}

func reconcileAccounts(ctx context.Context, tx *sql.Tx, recs map[string]s3Account, byBase map[string][]string, res *SyncResult) error {
	type accountRef struct {
		id  int64
		key string
	}

	var missing []accountRef
	rows, err := tx.QueryContext(ctx, `SELECT id, object_key FROM accounts WHERE status IN ('sold', 'reserved')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r accountRef
		if err := rows.Scan(&r.id, &r.key); err != nil {
			rows.Close()
			return err
		}
		if _, ok := recs[r.key]; !ok {
			missing = append(missing, r)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, r := range missing {
		keys := byBase[path.Base(r.key)]
		if len(keys) != 1 {
			res.Unresolved++
			continue
		}
		target := keys[0]
		rec := recs[target]
		if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE object_key = ? AND status = 'available'`, target); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE object_key = ?`, target).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			res.Unresolved++
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts
			SET object_key = ?, json_key = ?, account_name = ?, batch_prefix = ?, size_bytes = ?
			WHERE id = ?`,
			rec.Key, rec.JSONKey, rec.Name, rec.Batch, rec.Size, r.id); err != nil {
			return err
		}
		res.Remapped++
	}

	var stale []int64
	rows, err = tx.QueryContext(ctx, `SELECT id, object_key FROM accounts WHERE status = 'available'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r accountRef
		if err := rows.Scan(&r.id, &r.key); err != nil {
			rows.Close()
			return err
		}
		if _, ok := recs[r.key]; !ok {
			stale = append(stale, r.id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ? AND status = 'available'`, id); err != nil {
			return err
		}
		res.Pruned++
	}

	dres, err := tx.ExecContext(ctx, `DELETE a FROM accounts a
		JOIN accounts b ON SUBSTRING_INDEX(a.object_key, '/', -1) = SUBSTRING_INDEX(b.object_key, '/', -1)
			AND a.id <> b.id
		WHERE a.status = 'available' AND b.status IN ('sold', 'reserved')`)
	if err != nil {
		return err
	}
	if n, err := dres.RowsAffected(); err == nil {
		res.Duplicates = int(n)
	}
	return nil
}

func parseAccountDate(value string, loc *time.Location) time.Time {
	if len(value) < 6 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("060102", value[:6], loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *Store) Dates(ctx context.Context) ([]DateStock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_date, COUNT(*)
		FROM accounts WHERE status = 'available'
		GROUP BY account_date ORDER BY account_date`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DateStock
	for rows.Next() {
		var d time.Time
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out = append(out, DateStock{Date: d, Count: n})
	}
	return out, rows.Err()
}

func (s *Store) Available(ctx context.Context, date string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts
		WHERE account_date = ? AND status = 'available'`, date).Scan(&n)
	return n, err
}

func (s *Store) Reserve(ctx context.Context, orderID, idemKey string, chatID int64, username, date string, qty int, maxBytes, pricePerAccount int64) ([]Account, int64, *Order, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		accounts, total, duplicate, err := s.reserveOnce(ctx, orderID, idemKey, chatID, username, date, qty, maxBytes, pricePerAccount)
		if err == nil {
			return accounts, total, duplicate, nil
		}
		if !isRetryableMySQL(err) {
			return nil, 0, nil, err
		}
		lastErr = err
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	return nil, 0, nil, lastErr
}

func (s *Store) reserveOnce(ctx context.Context, orderID, idemKey string, chatID int64, username, date string, qty int, maxBytes, pricePerAccount int64) ([]Account, int64, *Order, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	defer tx.Rollback()

	if idemKey != "" {
		existing, err := getOrderByKeyTx(ctx, tx, idemKey)
		if err == nil {
			return nil, 0, existing, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil, err
		}
	}

	totalPrice := int64(qty) * pricePerAccount
	if _, err := tx.ExecContext(ctx, `INSERT INTO orders
		(id, chat_id, username, account_date, quantity, amount_bytes, price_per_account, total_price, status, idempotency_key)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, 'pending', ?)`,
		orderID, chatID, username, date, qty, pricePerAccount, totalPrice, idemKey); err != nil {
		var mysqlErr *mysql.MySQLError
		if idemKey != "" && errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			tx.Rollback()
			for i := 0; i < 10; i++ {
				existing, getErr := s.GetOrderByKey(ctx, idemKey)
				if getErr == nil {
					return nil, 0, existing, nil
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		return nil, 0, nil, err
	}

	res, err := tx.ExecContext(ctx, `UPDATE accounts
		SET status = 'reserved', order_id = ?, sold_to = ?, reserved_at = NOW()
		WHERE account_date = ? AND status = 'available'
		ORDER BY id LIMIT ?`, orderID, chatID, date, qty)
	if err != nil {
		return nil, 0, nil, err
	}
	claimed, err := res.RowsAffected()
	if err != nil {
		return nil, 0, nil, err
	}
	if claimed != int64(qty) {
		return nil, 0, nil, ErrInsufficient
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, object_key, json_key, account_name, batch_prefix, size_bytes
		FROM accounts
		WHERE order_id = ? AND status = 'reserved'`, orderID)
	if err != nil {
		return nil, 0, nil, err
	}
	var accounts []Account
	var total int64
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.ObjectKey, &a.JSONKey, &a.Name, &a.Batch, &a.Size); err != nil {
			rows.Close()
			return nil, 0, nil, err
		}
		total += a.Size
		accounts = append(accounts, a)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, nil, err
	}
	if len(accounts) != qty {
		return nil, 0, nil, ErrInsufficient
	}
	if maxBytes > 0 && total > maxBytes {
		return nil, total, nil, ErrTooLarge
	}

	if _, err := tx.ExecContext(ctx, `UPDATE orders SET amount_bytes = ? WHERE id = ?`, total, orderID); err != nil {
		return nil, 0, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, nil, err
	}
	return accounts, total, nil, nil
}

func isRetryableMySQL(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	return mysqlErr.Number == 1213 || mysqlErr.Number == 1205
}

func (s *Store) Finalize(ctx context.Context, orderID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE accounts
		SET status = 'sold', sold_at = NOW()
		WHERE order_id = ? AND status = 'reserved'`, orderID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE orders
		SET status = 'sent', paid_at = COALESCE(paid_at, NOW()), sent_at = NOW()
		WHERE id = ? AND status IN ('pending', 'paid', 'processing')`, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimDelivery(ctx context.Context, orderID string, chatID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET status = 'processing'
		WHERE id = ? AND chat_id = ? AND status IN ('pending', 'paid')`, orderID, chatID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Store) RevertDelivery(ctx context.Context, orderID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE orders SET status = 'paid'
		WHERE id = ? AND status = 'processing'`, orderID)
	return err
}

func (s *Store) SetInvoice(ctx context.Context, orderID string, inv *Invoice, expires time.Time) error {
	var exp any
	if !expires.IsZero() {
		exp = expires.UTC()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE orders
		SET invoice_id = ?, invoice_status = ?, payment_fee = ?, gateway_total = ?, qris_url = ?, invoice_expires_at = ?
		WHERE id = ?`,
		inv.InvoiceID, paymentStatus(inv.Status), inv.Fee, inv.Total, inv.QRISImage, exp, orderID)
	return err
}

func (s *Store) SetInvoiceStatus(ctx context.Context, orderID, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE orders SET invoice_status = ? WHERE id = ?`, status, orderID)
	return err
}

func (s *Store) PendingOrderByChat(ctx context.Context, chatID int64) (*Order, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE chat_id = ? AND status = 'pending' AND invoice_id IS NOT NULL AND invoice_id <> ''
		ORDER BY created_at DESC LIMIT 1`, chatID)
	return scanOrder(row)
}

func (s *Store) ListPendingInvoices(ctx context.Context) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE status = 'pending' AND invoice_id IS NOT NULL AND invoice_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

func (s *Store) OrderAccounts(ctx context.Context, orderID string) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, object_key, json_key, account_name, batch_prefix, size_bytes
		FROM accounts WHERE order_id = ? AND status IN ('reserved', 'sold') ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.ObjectKey, &a.JSONKey, &a.Name, &a.Batch, &a.Size); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	s.resolveAccountKeys(ctx, out)
	return out, nil
}

// resolveAccountKeys menangani file yang dipindahkan/diganti path di object
// storage setelah order dibuat. Pencocokan dilakukan lewat nama file.
func (s *Store) resolveAccountKeys(ctx context.Context, accounts []Account) {
	var missing []int
	for i := range accounts {
		if _, err := s.s3.StatObject(ctx, s.cfg.S3Bucket, accounts[i].ObjectKey, minio.StatObjectOptions{}); err != nil {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return
	}

	byBase := make(map[string]string)
	dup := make(map[string]bool)
	for obj := range s.s3.ListObjects(ctx, s.cfg.S3Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			log.Printf("resolve akun: list object: %v", obj.Err)
			return
		}
		if !strings.HasSuffix(obj.Key, ".tar.gz") {
			continue
		}
		base := path.Base(obj.Key)
		if _, ok := byBase[base]; ok {
			dup[base] = true
			continue
		}
		byBase[base] = obj.Key
	}

	for _, i := range missing {
		base := path.Base(accounts[i].ObjectKey)
		if dup[base] {
			continue
		}
		key, ok := byBase[base]
		if !ok {
			continue
		}
		log.Printf("akun %d: object %s tidak ditemukan, pakai %s", accounts[i].ID, accounts[i].ObjectKey, key)
		accounts[i].ObjectKey = key
		accounts[i].JSONKey = strings.TrimSuffix(key, ".tar.gz") + ".json"
	}
}

func (s *Store) Release(ctx context.Context, orderID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE accounts
		SET status = 'available', order_id = NULL, sold_to = NULL, reserved_at = NULL
		WHERE order_id = ? AND status IN ('reserved', 'sold')`, orderID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE orders
		SET status = 'cancelled'
		WHERE id = ? AND status IN ('pending', 'paid', 'processing', 'sent')`, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReleaseStale(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE accounts a
		JOIN orders o ON a.order_id = o.id
		SET a.status = 'available', a.order_id = NULL, a.sold_to = NULL, a.reserved_at = NULL
		WHERE a.status = 'reserved' AND o.status = 'pending' AND o.invoice_status <> 'review'
		AND (
			((o.invoice_id IS NULL OR o.invoice_id = '') AND (a.reserved_at IS NULL OR a.reserved_at < (NOW() - INTERVAL 15 MINUTE)))
			OR (o.invoice_expires_at IS NOT NULL AND o.invoice_expires_at < (NOW() - INTERVAL 5 MINUTE) AND o.invoice_status <> 'review')
			OR (o.invoice_id IS NOT NULL AND o.invoice_expires_at IS NULL AND o.created_at < (NOW() - INTERVAL 30 MINUTE))
		)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE orders
		SET status = 'cancelled', invoice_status = CASE WHEN invoice_id IS NOT NULL AND invoice_id <> '' THEN 'expired' ELSE invoice_status END
		WHERE status = 'pending' AND invoice_status <> 'review'
		AND (
			(invoice_id IS NULL OR invoice_id = '') AND created_at < (NOW() - INTERVAL 15 MINUTE)
			OR (invoice_expires_at IS NOT NULL AND invoice_expires_at < (NOW() - INTERVAL 5 MINUTE) AND invoice_status <> 'review')
		)`); err != nil {
		return err
	}
	return tx.Commit()
}

const orderColumns = `id, chat_id, username, account_date, quantity, amount_bytes,
	price_per_account, total_price, status,
	COALESCE(invoice_id, ''), invoice_status, payment_fee, gateway_total, COALESCE(qris_url, ''), invoice_expires_at,
	created_at, paid_at, sent_at`

func scanOrder(scanner interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	err := scanner.Scan(
		&o.ID, &o.ChatID, &o.Username, &o.Date, &o.Quantity, &o.AmountBytes,
		&o.PricePerAccount, &o.TotalPrice, &o.Status,
		&o.InvoiceID, &o.InvoiceStatus, &o.PaymentFee, &o.GatewayTotal, &o.QRISURL, &o.InvoiceExpiresAt,
		&o.CreatedAt, &o.PaidAt, &o.SentAt,
	)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (s *Store) GetOrder(ctx context.Context, id string) (*Order, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = ?`, id)
	return scanOrder(row)
}

func (s *Store) GetOrderByKey(ctx context.Context, key string) (*Order, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE idempotency_key = ?`, key)
	return scanOrder(row)
}

func getOrderByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*Order, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE idempotency_key = ?`, key)
	return scanOrder(row)
}

func (s *Store) ListOrders(ctx context.Context, chatID int64, limit int) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE chat_id = ? ORDER BY created_at DESC LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

func (s *Store) CancelOrder(ctx context.Context, id string, chatID int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM orders
		WHERE id = ? AND chat_id = ? FOR UPDATE`, id, chatID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != "pending" {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE accounts
		SET status = 'available', order_id = NULL, sold_to = NULL, reserved_at = NULL
		WHERE order_id = ? AND status = 'reserved'`, id); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE orders
		SET status = 'cancelled',
			invoice_status = CASE WHEN invoice_id IS NOT NULL AND invoice_id <> '' THEN 'cancelled' ELSE invoice_status END
		WHERE id = ?`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
