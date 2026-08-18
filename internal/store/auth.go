package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/delve8/agora/internal/auth"
)

type Device struct {
	ID             string
	UserID         string
	CredentialHash string
	Name           string
	RevokedAt      *time.Time
	CreatedAt      time.Time
	LastSeenAt     *time.Time
}

type PairCode struct {
	Hash       string
	UserID     string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
}

func HashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (s *Store) GetOrCreateLocalUser(ctx context.Context) (auth.Principal, error) {
	return s.provisionUser(ctx, auth.ProvisionClaims{Provider: auth.ProviderLocal, Subject: "local", DisplayName: "Local"})
}

func (s *Store) FindByAuthSubject(ctx context.Context, subject string) (auth.Principal, error) {
	return s.findPrincipal(ctx, auth.ProviderLogto, subject)
}

func (s *Store) FindByClaims(ctx context.Context, claims auth.ProvisionClaims) (auth.Principal, error) {
	if strings.TrimSpace(claims.Provider) == "" {
		claims.Provider = auth.ProviderLogto
	}
	return s.provisionUser(ctx, claims)
}

func (s *Store) findPrincipal(ctx context.Context, provider, subject string) (auth.Principal, error) {
	var p auth.Principal
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.display_name,u.email,ui.provider,ui.subject FROM users u JOIN user_identities ui ON ui.user_id=u.id WHERE ui.provider=? AND ui.subject=? AND u.status='active'`, provider, subject).Scan(&p.UserID, &p.DisplayName, &p.Email, &p.Provider, &p.AuthSubject)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Principal{}, sql.ErrNoRows
	}
	return p, err
}

func (s *Store) provisionUser(ctx context.Context, claims auth.ProvisionClaims) (auth.Principal, error) {
	provider := strings.TrimSpace(claims.Provider)
	subject := strings.TrimSpace(claims.Subject)
	if provider == "" || subject == "" {
		return auth.Principal{}, errors.New("identity provider and subject are required")
	}
	if existing, err := s.findPrincipal(ctx, provider, subject); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return auth.Principal{}, err
	}
	userID := "user-" + HashSecret(provider + "\x00" + subject)[:24]
	displayName := strings.TrimSpace(claims.DisplayName)
	if displayName == "" {
		displayName = subject
	}
	now := time.Now().UTC().Format(timeFormat)
	err := s.withBusyRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO users(id,display_name,email,status,created_at,last_seen_at) VALUES(?,?,?,'active',?,?) ON CONFLICT(id) DO UPDATE SET display_name=excluded.display_name,email=excluded.email,last_seen_at=excluded.last_seen_at`, userID, displayName, claims.Email, now, now)
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO user_identities(user_id,provider,subject) VALUES(?,?,?) ON CONFLICT(provider,subject) DO NOTHING`, userID, provider, subject)
		return err
	})
	if err != nil {
		return auth.Principal{}, err
	}
	return s.findPrincipal(ctx, provider, subject)
}

func (s *Store) CreatePairCode(ctx context.Context, userID, codeHash string, expiresAt time.Time) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(codeHash) == "" {
		return errors.New("user id and pair code hash are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pair_codes(code_hash,user_id,expires_at) VALUES(?,?,?)`, codeHash, userID, expiresAt.UTC().Format(timeFormat))
	return err
}

func (s *Store) ConsumePairCode(ctx context.Context, codeHash string, now time.Time) (string, error) {
	var userID, expires string
	err := s.db.QueryRowContext(ctx, `SELECT user_id,expires_at FROM pair_codes WHERE code_hash=? AND (consumed_at IS NULL OR consumed_at='')`, codeHash).Scan(&userID, &expires)
	if err != nil {
		return "", err
	}
	expiresAt, err := parseTime(expires)
	if err != nil || !expiresAt.After(now) {
		return "", errors.New("pairing code is expired")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE pair_codes SET consumed_at=? WHERE code_hash=? AND (consumed_at IS NULL OR consumed_at='')`, now.UTC().Format(timeFormat), codeHash)
	if err != nil {
		return "", err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return "", errors.New("pairing code was already consumed")
	}
	return userID, nil
}

func (s *Store) CreateDevice(ctx context.Context, device Device) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO devices(device_id,user_id,credential_hash,name,created_at,last_seen_at) VALUES(?,?,?,?,?,?)`, device.ID, device.UserID, device.CredentialHash, device.Name, device.CreatedAt.UTC().Format(timeFormat), formatOptionalTime(device.LastSeenAt))
	return err
}

func (s *Store) GetDeviceByCredentialHash(ctx context.Context, hash string) (Device, error) {
	var d Device
	var created, seen, revoked string
	err := s.db.QueryRowContext(ctx, `SELECT device_id,user_id,credential_hash,name,created_at,last_seen_at,revoked_at FROM devices WHERE credential_hash=?`, hash).Scan(&d.ID, &d.UserID, &d.CredentialHash, &d.Name, &created, &seen, &revoked)
	if err != nil {
		return d, err
	}
	d.CreatedAt, err = parseTime(created)
	if err != nil {
		return d, err
	}
	d.LastSeenAt, err = parseOptionalTime(seen)
	if err != nil {
		return d, err
	}
	d.RevokedAt, err = parseOptionalTime(revoked)
	return d, err
}

func (s *Store) TouchDevice(ctx context.Context, deviceID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen_at=? WHERE device_id=? AND (revoked_at IS NULL OR revoked_at='')`, at.UTC().Format(timeFormat), deviceID)
	return err
}

func (s *Store) RevokeDevice(ctx context.Context, deviceID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET revoked_at=? WHERE device_id=?`, at.UTC().Format(timeFormat), deviceID)
	return err
}

func (s *Store) UserOwnsDaemon(ctx context.Context, userID, daemonID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE user_id=? AND device_id=? AND (revoked_at IS NULL OR revoked_at='')`, userID, daemonID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && exists == 1, err
}

func (s *Store) ListDevices(ctx context.Context, userID string) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device_id,user_id,name,created_at,last_seen_at,revoked_at FROM devices WHERE user_id=? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Device
	for rows.Next() {
		var d Device
		var created, seen, revoked string
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &created, &seen, &revoked); err != nil {
			return nil, err
		}
		d.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		d.LastSeenAt, err = parseOptionalTime(seen)
		if err != nil {
			return nil, err
		}
		d.RevokedAt, err = parseOptionalTime(revoked)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
