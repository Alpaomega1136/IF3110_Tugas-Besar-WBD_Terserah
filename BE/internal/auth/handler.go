package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Hash dummy untuk mencegah timing attack saat username tidak ditemukan.
var dummyPasswordHash, _ = HashPassword("dummy_password_for_timing_defense")

type Service struct {
	store Store
	cfg   Config
}

func NewService(store Store, cfg Config) *Service {
	return &Service{
		store: store,
		cfg:   cfg.withDefaults(),
	}
}

func ValidateUsername(username string) (string, error) {
	u := strings.TrimSpace(username)
	if len(u) < 1 || len(u) > 100 || strings.ContainsAny(u, " \t\r\n") {
		return "", errors.New("username must be 1..100 characters without whitespace")
	}
	return u, nil
}

func (s *Service) BootstrapAdmin(ctx context.Context, username, password string) (User, error) {
	u, err := ValidateUsername(username)
	if err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	return s.store.BootstrapAdmin(ctx, u, hash, s.cfg.Now())
}

func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !checkOrigin(r) {
		writeError(w, http.StatusForbidden, "csrf_invalid", "Cross-origin request rejected")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid JSON payload")
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid_input", "Username and password are required")
		return
	}

	user, err := s.store.GetUserByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			_, _ = VerifyPassword(req.Password, dummyPasswordHash)
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid username or password")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Authentication failed")
		return
	}
	ok, err := VerifyPassword(req.Password, user.PasswordHash)
	if err != nil || !ok || !user.IsActive {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid username or password")
		return
	}

	now := s.cfg.Now()
	if oldCookie, err := r.Cookie(CookieName); err == nil && oldCookie.Value != "" {
		_ = s.store.RevokeSessionByToken(r.Context(), hashToken(oldCookie.Value), now)
	}

	rawToken, tokenHash, err := generateOpaqueToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot initialize session")
		return
	}
	_, initialCSRFHash, err := generateOpaqueToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot initialize session")
		return
	}

	absExpiry := now.Add(s.cfg.AbsoluteLifetime)
	if _, err := s.store.CreateSession(r.Context(), user.ID, tokenHash, initialCSRFHash, now, absExpiry); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot persist session")
		return
	}

	s.setSessionCookie(w, r, rawToken)
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	if err := s.store.RevokeSession(r.Context(), p.SessionID, s.cfg.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Logout failed")
		return
	}
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleMe(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": p})
}

func (s *Service) HandleCSRF(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	rawCSRF, csrfHash, err := generateOpaqueToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot generate CSRF token")
		return
	}
	if err := s.store.UpdateCSRFTokenHash(r.Context(), p.SessionID, csrfHash); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot store CSRF token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": rawCSRF})
}

func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(CookieName)
		if err != nil || strings.TrimSpace(cookie.Value) == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required")
			return
		}
		p, newRawToken, err := s.store.AuthenticateAndRotate(r.Context(), hashToken(cookie.Value), s.cfg.Now(), s.cfg)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				s.clearSessionCookie(w, r)
				writeError(w, http.StatusUnauthorized, "unauthenticated", "Session expired or invalid")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "Session validation failed")
			return
		}
		if newRawToken != "" {
			s.setSessionCookie(w, r, newRawToken)
		}

		if isStateChanging(r.Method) {
			if !checkOrigin(r) {
				writeError(w, http.StatusForbidden, "csrf_invalid", "Cross-origin request rejected")
				return
			}
			csrfHeader := strings.TrimSpace(r.Header.Get(CSRFHeader))
			if csrfHeader == "" || len(p.CSRFTokenHash) != 32 || subtle.ConstantTimeCompare(hashToken(csrfHeader), p.CSRFTokenHash) != 1 {
				writeError(w, http.StatusForbidden, "csrf_invalid", "Missing or invalid CSRF token")
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required")
			return
		}
		if !p.IsPlatformAdmin {
			writeError(w, http.StatusForbidden, "forbidden", "Platform Admin access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) isSecure(r *http.Request) bool {
	return s.cfg.CookieSecure || r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Service) setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.isSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.isSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("unexpected trailing data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
