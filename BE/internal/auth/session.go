package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/httpx"
	"terserah/backend/internal/secretbox"
)

const CookieName = "terserah_session"
const preloginCookie = "terserah_login_csrf"
const IdleTimeout = 30 * time.Minute
const AbsoluteTimeout = 8 * time.Hour
const RotationInterval = 15 * time.Minute
const GracePeriod = 30 * time.Second

var ErrUnauthenticated = errors.New("authentication required")

type Principal struct {
	ID              int64  `json:"id,string"`
	Username        string `json:"username"`
	IsPlatformAdmin bool   `json:"is_platform_admin"`
	sessionID       int64
	csrfHash        []byte
}

type principalKey struct{}

func User(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type Options struct {
	Origin string
	Key    []byte
	Now    func() time.Time
}

type Service struct {
	db        *pgxpool.Pool
	box       *secretbox.Box
	origin    string
	secure    bool
	csrfKey   []byte
	now       func() time.Time
	dummyHash string
}

func New(db *pgxpool.Pool, opts Options) (*Service, error) {
	box, err := secretbox.New(opts.Key)
	if err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	h := hmac.New(sha256.New, opts.Key)
	h.Write([]byte("terserah/csrf/v1"))
	dummy, err := HashPassword(randomToken())
	if err != nil {
		return nil, err
	}
	origin, err := url.Parse(opts.Origin)
	if err != nil {
		return nil, err
	}
	secure := origin.Scheme == "https" || (origin.Scheme == "http" && origin.Hostname() == "localhost")
	return &Service{db: db, box: box, origin: opts.Origin, secure: secure, csrfKey: h.Sum(nil), now: opts.Now, dummyHash: dummy}, nil
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func (s *Service) sign(value string) string {
	h := hmac.New(sha256.New, s.csrfKey)
	h.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s *Service) csrfToken(id int64) string { return s.sign(fmt.Sprintf("session:%d", id)) }

func (s *Service) cookie(w http.ResponseWriter, name, value string, expiry time.Time) {
	maxAge := int(expiry.Sub(s.now()).Seconds())
	if value == "" || maxAge <= 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, Expires: expiry, MaxAge: maxAge})
}

// Authenticate serializes rotation per session and reissues the current token
// when a request uses a still-valid grace alias (including a lost response).
func (s *Service) Authenticate(w http.ResponseWriter, r *http.Request) (Principal, error) {
	var p Principal
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return p, ErrUnauthenticated
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(raw) != 32 {
		s.cookie(w, CookieName, "", s.now())
		return p, ErrUnauthenticated
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	var lastSeen, absolute, rotated time.Time
	var active, adminBefore, rotationRequired bool
	var encrypted []byte
	now := s.now()
	err = tx.QueryRow(r.Context(), `SELECT s.id,u.id,u.username,u.is_platform_admin,u.is_active,s.csrf_token_hash,
		s.last_seen_at,s.absolute_expires_at,s.rotated_at,s.admin_at_login,s.rotation_required,s.current_token_encrypted
		FROM session_tokens t JOIN sessions s ON s.id=t.session_id JOIN users u ON u.id=s.user_id
		WHERE t.token_hash=$1 AND t.valid_until>$2 AND s.revoked_at IS NULL FOR UPDATE OF s`, tokenHash(cookie.Value), now).
		Scan(&p.sessionID, &p.ID, &p.Username, &p.IsPlatformAdmin, &active, &p.csrfHash, &lastSeen, &absolute, &rotated, &adminBefore, &rotationRequired, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		s.cookie(w, CookieName, "", now)
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	if !active || !now.Before(absolute) || !now.Before(lastSeen.Add(IdleTimeout)) || (adminBefore && !p.IsPlatformAdmin) || len(encrypted) == 0 {
		if _, err = tx.Exec(r.Context(), "UPDATE sessions SET revoked_at=$2 WHERE id=$1", p.sessionID, now); err != nil {
			return Principal{}, err
		}
		if err = tx.Commit(r.Context()); err != nil {
			return Principal{}, err
		}
		s.cookie(w, CookieName, "", now)
		return Principal{}, ErrUnauthenticated
	}
	current, err := s.box.Open(encrypted, fmt.Sprintf("session:%d", p.sessionID))
	if err != nil {
		return Principal{}, errors.New("cannot read current session token")
	}
	if rotationRequired || (!adminBefore && p.IsPlatformAdmin) || !now.Before(rotated.Add(RotationInterval)) {
		current = []byte(randomToken())
		graceUntil := now.Add(GracePeriod)
		if graceUntil.After(absolute) {
			graceUntil = absolute
		}
		if _, err = tx.Exec(r.Context(), "UPDATE session_tokens SET valid_until=LEAST(valid_until,$2) WHERE session_id=$1", p.sessionID, graceUntil); err != nil {
			return Principal{}, err
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO session_tokens(token_hash,session_id,valid_until) VALUES($1,$2,$3)", tokenHash(string(current)), p.sessionID, absolute); err != nil {
			return Principal{}, err
		}
		encrypted = s.box.Seal(current, fmt.Sprintf("session:%d", p.sessionID))
		if _, err = tx.Exec(r.Context(), "UPDATE sessions SET rotated_at=$2,current_token_encrypted=$3,admin_at_login=$4,rotation_required=false WHERE id=$1", p.sessionID, now, encrypted, p.IsPlatformAdmin); err != nil {
			return Principal{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "UPDATE sessions SET last_seen_at=$2 WHERE id=$1", p.sessionID, now); err != nil {
		return Principal{}, err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return Principal{}, err
	}
	if subtle.ConstantTimeCompare(current, []byte(cookie.Value)) != 1 {
		s.cookie(w, CookieName, string(current), absolute)
	}
	return p, nil
}

// ChangeUserAccess must be called in the same metadata transaction as a grant
// or membership change. A privilege increase requests rotation; revocation
// invalidates every alias immediately.
func ChangeUserAccess(ctx context.Context, tx pgx.Tx, userID int64, increase bool) error {
	if increase {
		_, err := tx.Exec(ctx, "UPDATE sessions SET rotation_required=true WHERE user_id=$1 AND revoked_at IS NULL", userID)
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL", userID)
	return err
}

func (s *Service) validOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == s.origin && r.Header.Get("Sec-Fetch-Site") != "cross-site"
}

func (s *Service) validCSRF(r *http.Request, p Principal) bool {
	value := r.Header.Get("X-CSRF-Token")
	return value != "" && s.validOrigin(r) && subtle.ConstantTimeCompare(tokenHash(value), p.csrfHash) == 1
}

func (s *Service) Require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		p, err := s.Authenticate(w, r)
		if err != nil {
			s.authError(w, err)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && !s.validCSRF(r, p) {
			httpx.Error(w, 403, "csrf_rejected", "CSRF validation failed")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

func (s *Service) authError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnauthenticated) {
		httpx.Error(w, 401, "unauthenticated", "Login required")
		return
	}
	httpx.Error(w, 503, "database_unavailable", "Authentication is unavailable")
}

func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/auth/csrf", s.csrf)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("GET /api/v1/auth/me", s.Require(func(w http.ResponseWriter, r *http.Request) { p, _ := User(r.Context()); httpx.JSON(w, 200, p) }))
	mux.HandleFunc("POST /api/v1/auth/logout", s.Require(s.logout))
}

func (s *Service) csrf(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != s.origin) {
		httpx.Error(w, 403, "csrf_rejected", "Origin rejected")
		return
	}
	p, err := s.Authenticate(w, r)
	if err == nil {
		httpx.JSON(w, 200, map[string]string{"csrf_token": s.csrfToken(p.sessionID)})
		return
	}
	if !errors.Is(err, ErrUnauthenticated) {
		s.authError(w, err)
		return
	}
	expires := s.now().Add(10 * time.Minute)
	payload := randomToken() + "." + strconv.FormatInt(expires.Unix(), 10)
	token := payload + "." + s.sign("login:"+payload)
	s.cookie(w, preloginCookie, token, expires)
	httpx.JSON(w, 200, map[string]string{"csrf_token": token})
}

func (s *Service) validPrelogin(r *http.Request) bool {
	if !s.validOrigin(r) {
		return false
	}
	c, err := r.Cookie(preloginCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && s.now().Unix() < expiry && subtle.ConstantTimeCompare([]byte(parts[2]), []byte(s.sign("login:"+parts[0]+"."+parts[1]))) == 1
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	valid := s.validPrelogin(r)
	if !valid {
		if p, err := s.Authenticate(w, r); err == nil {
			valid = s.validCSRF(r, p)
		} else if !errors.Is(err, ErrUnauthenticated) {
			s.authError(w, err)
			return
		}
	}
	if !valid {
		httpx.Error(w, 403, "csrf_rejected", "CSRF validation failed")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if len(input.Username) < 1 || len(input.Username) > 100 || len(input.Password) > 1024 {
		httpx.Error(w, 400, "invalid_input", "Invalid credentials format")
		return
	}
	var p Principal
	var passwordHash string
	var active bool
	err := s.db.QueryRow(r.Context(), "SELECT id,username,is_platform_admin,is_active,password_hash FROM users WHERE username=$1", input.Username).Scan(&p.ID, &p.Username, &p.IsPlatformAdmin, &active, &passwordHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.authError(w, err)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		passwordHash = s.dummyHash
	}
	if !CheckPassword(passwordHash, input.Password) || !active {
		httpx.Error(w, 401, "invalid_credentials", "Invalid username or password")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.authError(w, err)
		return
	}
	defer tx.Rollback(context.Background())
	// Recheck the account inside the transaction before creating a session.
	var latestHash string
	err = tx.QueryRow(r.Context(), "SELECT is_platform_admin,password_hash FROM users WHERE id=$1 AND is_active=true FOR UPDATE", p.ID).Scan(&p.IsPlatformAdmin, &latestHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.authError(w, ErrUnauthenticated)
		} else {
			s.authError(w, err)
		}
		return
	}
	if latestHash != passwordHash && !CheckPassword(latestHash, input.Password) {
		httpx.Error(w, 401, "invalid_credentials", "Invalid username or password")
		return
	}
	passwordHash = latestHash
	if strings.HasPrefix(passwordHash, "$pbkdf2-sha256$i=210000$") {
		upgraded, upgradeErr := canonicalPasswordHash(input.Password)
		if upgradeErr != nil {
			s.authError(w, upgradeErr)
			return
		}
		if _, err = tx.Exec(r.Context(), "UPDATE users SET password_hash=$2 WHERE id=$1", p.ID, upgraded); err != nil {
			s.authError(w, err)
			return
		}
	}
	if c, e := r.Cookie(CookieName); e == nil {
		if _, err = tx.Exec(r.Context(), "UPDATE sessions SET revoked_at=$2 WHERE id IN (SELECT session_id FROM session_tokens WHERE token_hash=$1)", tokenHash(c.Value), s.now()); err != nil {
			s.authError(w, err)
			return
		}
	}
	now := s.now()
	absolute := now.Add(AbsoluteTimeout)
	// The placeholder and encrypted token are written in one transaction.
	err = tx.QueryRow(r.Context(), `INSERT INTO sessions(user_id,csrf_token_hash,created_at,last_seen_at,absolute_expires_at,rotated_at,admin_at_login)
		VALUES($1,$2,$3,$3,$4,$3,$5) RETURNING id`, p.ID, []byte{}, now, absolute, p.IsPlatformAdmin).Scan(&p.sessionID)
	if err != nil {
		s.authError(w, err)
		return
	}
	token := randomToken()
	if _, err = tx.Exec(r.Context(), "UPDATE sessions SET csrf_token_hash=$2,current_token_encrypted=$3 WHERE id=$1", p.sessionID, tokenHash(s.csrfToken(p.sessionID)), s.box.Seal([]byte(token), fmt.Sprintf("session:%d", p.sessionID))); err != nil {
		s.authError(w, err)
		return
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO session_tokens(token_hash,session_id,valid_until) VALUES($1,$2,$3)", tokenHash(token), p.sessionID, absolute); err != nil {
		s.authError(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.authError(w, err)
		return
	}
	s.cookie(w, CookieName, token, absolute)
	s.cookie(w, preloginCookie, "", now)
	httpx.JSON(w, 200, p)
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := User(r.Context())
	if _, err := s.db.Exec(r.Context(), "UPDATE sessions SET revoked_at=$2 WHERE id=$1", p.sessionID, s.now()); err != nil {
		s.authError(w, err)
		return
	}
	s.cookie(w, CookieName, "", s.now())
	w.WriteHeader(http.StatusNoContent)
}
