package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	CookieName              = "terserah_session"
	CSRFHeader              = "X-CSRF-Token"
	DefaultIdleTimeout      = 30 * time.Minute
	DefaultAbsoluteLifetime = 8 * time.Hour
	DefaultRotationInterval = 15 * time.Minute
	DefaultGracePeriod      = 30 * time.Second
	tokenByteLen            = 32
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrUnauthenticated    = errors.New("unauthenticated")
	ErrForbidden          = errors.New("forbidden")
	ErrCSRFInvalid        = errors.New("invalid csrf token")
	ErrInvalidInput       = errors.New("invalid input")
)

type Config struct {
	IdleTimeout      time.Duration
	AbsoluteLifetime time.Duration
	RotationInterval time.Duration
	GracePeriod      time.Duration
	CookieSecure     bool
	Now              func() time.Time
}

func (c Config) withDefaults() Config {
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.AbsoluteLifetime <= 0 {
		c.AbsoluteLifetime = DefaultAbsoluteLifetime
	}
	if c.RotationInterval <= 0 {
		c.RotationInterval = DefaultRotationInterval
	}
	if c.GracePeriod <= 0 {
		c.GracePeriod = DefaultGracePeriod
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	return c
}

type User struct {
	ID              int64     `json:"id,string"`
	Username        string    `json:"username"`
	PasswordHash    string    `json:"-"`
	IsPlatformAdmin bool      `json:"is_platform_admin"`
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
}

type Principal struct {
	UserID          int64  `json:"id,string"`
	Username        string `json:"username"`
	IsPlatformAdmin bool   `json:"is_platform_admin"`
	IsActive        bool   `json:"is_active"`
	SessionID       int64  `json:"-"`
	CSRFTokenHash   []byte `json:"-"`
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

func generateOpaqueToken() (string, []byte, error) {
	b := make([]byte, tokenByteLen)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	raw := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

type Store interface {
	BootstrapAdmin(ctx context.Context, username, passwordHash string, now time.Time) (User, error)
	GetUserByUsername(ctx context.Context, username string) (User, error)
	CreateSession(ctx context.Context, userID int64, tokenHash, csrfHash []byte, now, absoluteExpiresAt time.Time) (int64, error)
	AuthenticateAndRotate(ctx context.Context, tokenHash []byte, now time.Time, cfg Config) (Principal, string, error)
	UpdateCSRFTokenHash(ctx context.Context, sessionID int64, csrfHash []byte) error
	RevokeSession(ctx context.Context, sessionID int64, now time.Time) error
	RevokeSessionByToken(ctx context.Context, tokenHash []byte, now time.Time) error
	RevokeUserSessions(ctx context.Context, userID int64, now time.Time) error
	MarkUserSessionsForRotation(ctx context.Context, userID int64) error
}

// Execer abstraksi pool dan tx agar pencabutan/rotasi session dapat berjalan dalam transaksi yang sama.
type Execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// RevokeUserSessionsExec mencabut semua session aktif dan menghapus token milik pengguna.
func RevokeUserSessionsExec(ctx context.Context, db Execer, userID int64, now time.Time) error {
	if _, err := db.Exec(ctx, `DELETE FROM session_tokens WHERE session_id IN (SELECT id FROM sessions WHERE user_id = $1)`, userID); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, now)
	return err
}

// MarkUserSessionsForRotationExec menandai session aktif pengguna agar dirotasi pada request berikutnya.
func MarkUserSessionsForRotationExec(ctx context.Context, db Execer, userID int64) error {
	epoch := time.Unix(0, 0).UTC()
	_, err := db.Exec(ctx, `UPDATE sessions SET rotated_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, epoch)
	return err
}

// RevokeGroupSessionsExec mencabut semua session aktif seluruh anggota group.
func RevokeGroupSessionsExec(ctx context.Context, db Execer, groupID int64, now time.Time) error {
	if _, err := db.Exec(ctx, `
		DELETE FROM session_tokens
		WHERE session_id IN (
			SELECT s.id FROM sessions s
			JOIN user_groups ug ON ug.user_id = s.user_id
			WHERE ug.group_id = $1
		)`, groupID); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `
		UPDATE sessions SET revoked_at = $2
		WHERE user_id IN (SELECT user_id FROM user_groups WHERE group_id = $1)
		  AND revoked_at IS NULL`, groupID, now)
	return err
}

// MarkGroupSessionsForRotationExec menandai session aktif seluruh anggota group agar dirotasi pada request berikutnya.
func MarkGroupSessionsForRotationExec(ctx context.Context, db Execer, groupID int64) error {
	epoch := time.Unix(0, 0).UTC()
	_, err := db.Exec(ctx, `
		UPDATE sessions SET rotated_at = $2
		WHERE user_id IN (SELECT user_id FROM user_groups WHERE group_id = $1)
		  AND revoked_at IS NULL`, groupID, epoch)
	return err
}

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) BootstrapAdmin(ctx context.Context, username, passwordHash string, now time.Time) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, is_platform_admin, is_active, created_at)
		VALUES ($1, $2, true, true, $3)
		ON CONFLICT (username) DO UPDATE
		SET password_hash = EXCLUDED.password_hash,
		    is_platform_admin = true,
		    is_active = true
		RETURNING id, username, password_hash, is_platform_admin, is_active, created_at
	`, username, passwordHash, now).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsPlatformAdmin, &u.IsActive, &u.CreatedAt)
	return u, err
}

func (s *PostgresStore) GetUserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, username, password_hash, is_platform_admin, is_active, created_at
		FROM users
		WHERE username = $1
	`, username).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsPlatformAdmin, &u.IsActive, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	return u, err
}

func (s *PostgresStore) CreateSession(ctx context.Context, userID int64, tokenHash, csrfHash []byte, now, absoluteExpiresAt time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())

	var sessionID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO sessions (user_id, csrf_token_hash, created_at, last_seen_at, absolute_expires_at, rotated_at)
		VALUES ($1, $2, $3, $3, $4, $3)
		RETURNING id
	`, userID, csrfHash, now, absoluteExpiresAt).Scan(&sessionID)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO session_tokens (token_hash, session_id, valid_until)
		VALUES ($1, $2, $3)
	`, tokenHash, sessionID, absoluteExpiresAt)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return sessionID, nil
}

func (s *PostgresStore) AuthenticateAndRotate(ctx context.Context, tokenHash []byte, now time.Time, cfg Config) (Principal, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Principal{}, "", err
	}
	defer tx.Rollback(context.Background())

	var (
		p                 Principal
		lastSeenAt        time.Time
		absoluteExpiresAt time.Time
		rotatedAt         time.Time
		revokedAt         *time.Time
		tokenValidUntil   time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT s.id, s.user_id, s.csrf_token_hash, s.last_seen_at,
		       s.absolute_expires_at, s.rotated_at, s.revoked_at,
		       st.valid_until,
		       u.username, u.is_platform_admin, u.is_active
		FROM session_tokens st
		JOIN sessions s ON s.id = st.session_id
		JOIN users u ON u.id = s.user_id
		WHERE st.token_hash = $1
		FOR UPDATE OF s, st
	`, tokenHash).Scan(
		&p.SessionID, &p.UserID, &p.CSRFTokenHash, &lastSeenAt,
		&absoluteExpiresAt, &rotatedAt, &revokedAt,
		&tokenValidUntil,
		&p.Username, &p.IsPlatformAdmin, &p.IsActive,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, "", ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, "", err
	}

	if revokedAt != nil || !p.IsActive || !now.Before(tokenValidUntil) || !now.Before(absoluteExpiresAt) || now.Sub(lastSeenAt) >= cfg.IdleTimeout {
		return Principal{}, "", ErrUnauthenticated
	}

	var newRawToken string
	isPrimaryToken := tokenValidUntil.Equal(absoluteExpiresAt)
	if isPrimaryToken && now.Sub(rotatedAt) >= cfg.RotationInterval {
		raw, newHash, err := generateOpaqueToken()
		if err != nil {
			return Principal{}, "", err
		}
		graceUntil := now.Add(cfg.GracePeriod)
		if graceUntil.After(absoluteExpiresAt) {
			graceUntil = absoluteExpiresAt
		}
		if _, err := tx.Exec(ctx, `UPDATE session_tokens SET valid_until = $2 WHERE token_hash = $1`, tokenHash, graceUntil); err != nil {
			return Principal{}, "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO session_tokens (token_hash, session_id, valid_until) VALUES ($1, $2, $3)`, newHash, p.SessionID, absoluteExpiresAt); err != nil {
			return Principal{}, "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET rotated_at = $2, last_seen_at = $2 WHERE id = $1`, p.SessionID, now); err != nil {
			return Principal{}, "", err
		}
		newRawToken = raw
	} else {
		if _, err := tx.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, p.SessionID, now); err != nil {
			return Principal{}, "", err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Principal{}, "", err
	}
	return p, newRawToken, nil
}

func (s *PostgresStore) UpdateCSRFTokenHash(ctx context.Context, sessionID int64, csrfHash []byte) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET csrf_token_hash = $2 WHERE id = $1 AND revoked_at IS NULL`, sessionID, csrfHash)
	return err
}

func (s *PostgresStore) RevokeSession(ctx context.Context, sessionID int64, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())

	if _, err := tx.Exec(ctx, `DELETE FROM session_tokens WHERE session_id = $1`, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, sessionID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) RevokeSessionByToken(ctx context.Context, tokenHash []byte, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())

	var sessionID int64
	err = tx.QueryRow(ctx, `SELECT session_id FROM session_tokens WHERE token_hash = $1`, tokenHash).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM session_tokens WHERE session_id = $1`, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, sessionID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) RevokeUserSessions(ctx context.Context, userID int64, now time.Time) error {
	return RevokeUserSessionsExec(ctx, s.pool, userID, now)
}

func (s *PostgresStore) MarkUserSessionsForRotation(ctx context.Context, userID int64) error {
	return MarkUserSessionsForRotationExec(ctx, s.pool, userID)
}

// MemoryStore implementasi Store in-memory untuk pengujian unit auth dan session.
type MemoryStore struct {
	mu       sync.Mutex
	nextUID  int64
	nextSID  int64
	users    map[int64]User
	sessions map[int64]*memSession
	tokens   map[string]*memToken
}

type memSession struct {
	id                int64
	userID            int64
	csrfTokenHash     []byte
	createdAt         time.Time
	lastSeenAt        time.Time
	absoluteExpiresAt time.Time
	rotatedAt         time.Time
	revokedAt         *time.Time
}

type memToken struct {
	tokenHash  []byte
	sessionID  int64
	validUntil time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users:    make(map[int64]User),
		sessions: make(map[int64]*memSession),
		tokens:   make(map[string]*memToken),
	}
}

func (m *MemoryStore) PutUser(u User) User {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u.ID == 0 {
		m.nextUID++
		u.ID = m.nextUID
	} else if u.ID > m.nextUID {
		m.nextUID = u.ID
	}
	m.users[u.ID] = u
	return u
}

func (m *MemoryStore) BootstrapAdmin(_ context.Context, username, passwordHash string, now time.Time) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, u := range m.users {
		if u.Username == username {
			u.PasswordHash = passwordHash
			u.IsPlatformAdmin = true
			u.IsActive = true
			m.users[id] = u
			return u, nil
		}
	}
	m.nextUID++
	u := User{
		ID:              m.nextUID,
		Username:        username,
		PasswordHash:    passwordHash,
		IsPlatformAdmin: true,
		IsActive:        true,
		CreatedAt:       now,
	}
	m.users[u.ID] = u
	return u, nil
}

func (m *MemoryStore) GetUserByUsername(_ context.Context, username string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Username == username {
			return u, nil
		}
	}
	return User{}, ErrInvalidCredentials
}

func (m *MemoryStore) CreateSession(_ context.Context, userID int64, tokenHash, csrfHash []byte, now, absoluteExpiresAt time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSID++
	sid := m.nextSID
	cpCSRF := append([]byte(nil), csrfHash...)
	cpTok := append([]byte(nil), tokenHash...)
	m.sessions[sid] = &memSession{
		id:                sid,
		userID:            userID,
		csrfTokenHash:     cpCSRF,
		createdAt:         now,
		lastSeenAt:        now,
		absoluteExpiresAt: absoluteExpiresAt,
		rotatedAt:         now,
	}
	m.tokens[string(cpTok)] = &memToken{
		tokenHash:  cpTok,
		sessionID:  sid,
		validUntil: absoluteExpiresAt,
	}
	return sid, nil
}

func (m *MemoryStore) AuthenticateAndRotate(_ context.Context, tokenHash []byte, now time.Time, cfg Config) (Principal, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tok, ok := m.tokens[string(tokenHash)]
	if !ok {
		return Principal{}, "", ErrUnauthenticated
	}
	sess, ok := m.sessions[tok.sessionID]
	if !ok {
		return Principal{}, "", ErrUnauthenticated
	}
	u, ok := m.users[sess.userID]
	if !ok {
		return Principal{}, "", ErrUnauthenticated
	}

	if sess.revokedAt != nil || !u.IsActive || !now.Before(tok.validUntil) || !now.Before(sess.absoluteExpiresAt) || now.Sub(sess.lastSeenAt) >= cfg.IdleTimeout {
		return Principal{}, "", ErrUnauthenticated
	}

	var newRawToken string
	if tok.validUntil.Equal(sess.absoluteExpiresAt) && now.Sub(sess.rotatedAt) >= cfg.RotationInterval {
		raw, newHash, err := generateOpaqueToken()
		if err != nil {
			return Principal{}, "", err
		}
		graceUntil := now.Add(cfg.GracePeriod)
		if graceUntil.After(sess.absoluteExpiresAt) {
			graceUntil = sess.absoluteExpiresAt
		}
		tok.validUntil = graceUntil
		cpNew := append([]byte(nil), newHash...)
		m.tokens[string(cpNew)] = &memToken{
			tokenHash:  cpNew,
			sessionID:  sess.id,
			validUntil: sess.absoluteExpiresAt,
		}
		sess.rotatedAt = now
		sess.lastSeenAt = now
		newRawToken = raw
	} else {
		sess.lastSeenAt = now
	}

	return Principal{
		UserID:          u.ID,
		Username:        u.Username,
		IsPlatformAdmin: u.IsPlatformAdmin,
		IsActive:        u.IsActive,
		SessionID:       sess.id,
		CSRFTokenHash:   append([]byte(nil), sess.csrfTokenHash...),
	}, newRawToken, nil
}

func (m *MemoryStore) UpdateCSRFTokenHash(_ context.Context, sessionID int64, csrfHash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess, ok := m.sessions[sessionID]; ok && sess.revokedAt == nil {
		sess.csrfTokenHash = append([]byte(nil), csrfHash...)
	}
	return nil
}

func (m *MemoryStore) RevokeSession(_ context.Context, sessionID int64, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess, ok := m.sessions[sessionID]; ok {
		t := now
		sess.revokedAt = &t
	}
	for k, tok := range m.tokens {
		if tok.sessionID == sessionID {
			delete(m.tokens, k)
		}
	}
	return nil
}

func (m *MemoryStore) RevokeSessionByToken(_ context.Context, tokenHash []byte, now time.Time) error {
	m.mu.Lock()
	tok, ok := m.tokens[string(tokenHash)]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	sid := tok.sessionID
	m.mu.Unlock()
	return m.RevokeSession(context.Background(), sid, now)
}

func (m *MemoryStore) RevokeUserSessions(_ context.Context, userID int64, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sid, sess := range m.sessions {
		if sess.userID == userID {
			t := now
			sess.revokedAt = &t
			for k, tok := range m.tokens {
				if tok.sessionID == sid {
					delete(m.tokens, k)
				}
			}
		}
	}
	return nil
}

func (m *MemoryStore) MarkUserSessionsForRotation(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	epoch := time.Unix(0, 0).UTC()
	for _, sess := range m.sessions {
		if sess.userID == userID && sess.revokedAt == nil {
			sess.rotatedAt = epoch
		}
	}
	return nil
}
