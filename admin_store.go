package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const settingsKeyBotToken = "bot_token"
const settingsKeyAdminIDs = "admin_ids"
const settingsKeyPrice = "price_per_account"
const settingsKeyMaxAccounts = "max_accounts_per_order"
const settingsKeyMaxZipBytes = "max_zip_bytes"
const settingsKeyHomeBanner = "home_banner_url"

var adminMigrations = []string{
	`CREATE TABLE IF NOT EXISTS settings (
		k VARCHAR(64) PRIMARY KEY,
		v TEXT NOT NULL,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE IF NOT EXISTS admin_users (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		username VARCHAR(64) NOT NULL,
		password_hash VARCHAR(255) NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		UNIQUE KEY uniq_admin_username (username)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE IF NOT EXISTS admin_sessions (
		token CHAR(64) PRIMARY KEY,
		user_id BIGINT NOT NULL,
		csrf CHAR(64) NOT NULL,
		expires_at DATETIME NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		KEY idx_admin_sessions_expires (expires_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

func ensureAdminSchema(ctx context.Context, db *sql.DB) error {
	for _, m := range adminMigrations {
		if _, err := db.ExecContext(ctx, m); err != nil {
			return fmt.Errorf("migrasi admin: %w", err)
		}
	}
	return nil
}

type AdminStats struct {
	TotalOrders   int
	Pending       int
	Review        int
	Processing    int
	Sent          int
	Cancelled     int
	TotalAccounts int
	Available     int
	Reserved      int
	Sold          int
}

type AdminStockRow struct {
	Date      time.Time
	Total     int
	Available int
	Reserved  int
	Sold      int
}

type AdminUser struct {
	ID           int64
	Username     string
	PasswordHash string
}

type AdminSession struct {
	Token     string
	UserID    int64
	Username  string
	CSRF      string
	ExpiresAt time.Time
}

func (s *Store) LoadSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT k, v FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) SeedSettings(ctx context.Context, defaults map[string]string) error {
	for k, v := range defaults {
		if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO settings (k, v) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetSettings(ctx context.Context, values map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for k, v := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (k, v) VALUES (?, ?)
			ON DUPLICATE KEY UPDATE v = VALUES(v)`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CountAdminUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n)
	return n, err
}

func (s *Store) CreateAdminUser(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_users (username, password_hash) VALUES (?, ?)`,
		username, passwordHash)
	return err
}

func (s *Store) AdminUserByUsername(ctx context.Context, username string) (*AdminUser, error) {
	var u AdminUser
	err := s.db.QueryRowContext(ctx, `SELECT id, username, password_hash FROM admin_users WHERE username = ?`,
		username).Scan(&u.ID, &u.Username, &u.PasswordHash)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) UpdateAdminPassword(ctx context.Context, id int64, hash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE admin_users SET password_hash = ? WHERE id = ?`, hash, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CreateAdminSession(ctx context.Context, token, csrf string, userID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_sessions (token, user_id, csrf, expires_at)
		VALUES (?, ?, ?, ?)`, token, userID, csrf, expires.UTC())
	return err
}

func (s *Store) GetAdminSession(ctx context.Context, token string) (*AdminSession, error) {
	var sess AdminSession
	err := s.db.QueryRowContext(ctx, `SELECT s.token, s.user_id, u.username, s.csrf, s.expires_at
		FROM admin_sessions s JOIN admin_users u ON u.id = s.user_id
		WHERE s.token = ?`, token).Scan(&sess.Token, &sess.UserID, &sess.Username, &sess.CSRF, &sess.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Store) TouchAdminSession(ctx context.Context, token string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE admin_sessions SET expires_at = ? WHERE token = ?`, expires.UTC(), token)
	return err
}

func (s *Store) DeleteAdminSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token = ?`, token)
	return err
}

func (s *Store) DeleteOtherAdminSessions(ctx context.Context, userID int64, keepToken string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE user_id = ? AND token <> ?`, userID, keepToken)
	return err
}

func (s *Store) DeleteExpiredAdminSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < UTC_TIMESTAMP()`)
	return err
}

func (s *Store) AdminStats(ctx context.Context) (*AdminStats, error) {
	var st AdminStats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(status = 'pending'), 0),
		COALESCE(SUM(status = 'pending' AND invoice_status = 'review'), 0),
		COALESCE(SUM(status = 'processing'), 0),
		COALESCE(SUM(status = 'sent'), 0),
		COALESCE(SUM(status = 'cancelled'), 0)
		FROM orders`).Scan(&st.TotalOrders, &st.Pending, &st.Review, &st.Processing, &st.Sent, &st.Cancelled); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(status = 'available'), 0),
		COALESCE(SUM(status = 'reserved'), 0),
		COALESCE(SUM(status = 'sold'), 0)
		FROM accounts`).Scan(&st.TotalAccounts, &st.Available, &st.Reserved, &st.Sold); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Store) AdminStock(ctx context.Context) ([]AdminStockRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_date, COUNT(*),
		COALESCE(SUM(status = 'available'), 0),
		COALESCE(SUM(status = 'reserved'), 0),
		COALESCE(SUM(status = 'sold'), 0)
		FROM accounts GROUP BY account_date ORDER BY account_date`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AdminStockRow
	for rows.Next() {
		var r AdminStockRow
		if err := rows.Scan(&r.Date, &r.Total, &r.Available, &r.Reserved, &r.Sold); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) AdminOrders(ctx context.Context, q, status, invoice string, limit, offset int) ([]Order, int, error) {
	where := []string{"1 = 1"}
	args := []any{}

	if q = strings.TrimSpace(q); q != "" {
		like := "%" + q + "%"
		where = append(where, "(id LIKE ? OR username LIKE ? OR CAST(chat_id AS CHAR) LIKE ?)")
		args = append(args, like, like, like)
	}
	if status = strings.TrimSpace(status); status != "" {
		where = append(where, "status = ?")
		args = append(args, status)
	}
	if invoice = strings.TrimSpace(invoice); invoice != "" {
		where = append(where, "invoice_status = ?")
		args = append(args, invoice)
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE `+cond+` ORDER BY created_at DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *o)
	}
	return out, total, rows.Err()
}

// MarkPaid memaksa order menjadi lunas tanpa menunggu konfirmasi Nexus.
func (s *Store) MarkPaid(ctx context.Context, orderID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE orders
		SET status = 'paid', invoice_status = 'paid', paid_at = COALESCE(paid_at, NOW())
		WHERE id = ? AND status IN ('pending', 'paid', 'processing')`, orderID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders
		WHERE id = ? AND status IN ('pending', 'paid', 'processing')`, orderID).Scan(&exists); err != nil {
		return false, err
	}
	return exists > 0, nil
}

func (s *Store) UpdateOrderInvoice(ctx context.Context, orderID string, inv *Invoice) error {
	_, err := s.db.ExecContext(ctx, `UPDATE orders
		SET invoice_status = ?,
			payment_fee = IF(? > 0, ?, payment_fee),
			gateway_total = IF(? > 0, ?, gateway_total)
		WHERE id = ?`, paymentStatus(inv.Status), inv.Fee, inv.Fee, inv.Total, inv.Total, orderID)
	return err
}

func hashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func verifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func parseAdminIDs(value string) []int64 {
	var out []int64
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if id, err := strconv.ParseInt(part, 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func joinIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func settingsInt(m map[string]string, key string, def int64) int64 {
	v := strings.TrimSpace(m[key])
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func applySettings(cfg *Config, m map[string]string) {
	if v := strings.TrimSpace(m[settingsKeyBotToken]); v != "" {
		cfg.BotToken = v
	}
	if v, ok := m[settingsKeyAdminIDs]; ok {
		cfg.AdminIDs = parseAdminIDs(v)
	}
	if v := settingsInt(m, settingsKeyPrice, 0); v > 0 {
		cfg.PricePerAccount = v
	}
	if v := settingsInt(m, settingsKeyMaxAccounts, 0); v > 0 {
		cfg.MaxAccounts = int(v)
	}
	if v := settingsInt(m, settingsKeyMaxZipBytes, 0); v > 0 {
		cfg.MaxZipBytes = v
	}
	if v, ok := m[settingsKeyHomeBanner]; ok {
		cfg.HomeBannerURL = strings.TrimSpace(v)
	}
}

func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
