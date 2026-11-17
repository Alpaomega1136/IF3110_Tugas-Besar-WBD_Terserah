package connections

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/policy"
	"terserah/backend/internal/secretbox"
)

var (
	ErrForbidden         = errors.New("access denied")
	ErrNotFound          = errors.New("database not found")
	ErrInvalid           = errors.New("invalid connection configuration")
	ErrConflict          = errors.New("connection conflicts with existing data")
	ErrTargetUnavailable = errors.New("cannot connect to target database")
	ErrMetadataTarget    = errors.New("metadata database cannot be used as a target")
)

type Connection struct {
	ID                int64         `json:"id,string"`
	Name              string        `json:"name"`
	Host              string        `json:"host"`
	Port              int           `json:"port"`
	DatabaseName      string        `json:"database_name"`
	Username          string        `json:"username"`
	SSLMode           string        `json:"sslmode"`
	CreatedAt         time.Time     `json:"created_at"`
	Access            policy.Access `json:"access,omitempty"`
	passwordEncrypted []byte
}

type Input struct {
	Name         string  `json:"name"`
	Host         string  `json:"host"`
	Port         int     `json:"port"`
	DatabaseName string  `json:"database_name"`
	Username     string  `json:"username"`
	Password     *string `json:"password"`
	SSLMode      string  `json:"sslmode"`
}

type Patch struct {
	Name         *string `json:"name"`
	Host         *string `json:"host"`
	Port         *int    `json:"port"`
	DatabaseName *string `json:"database_name"`
	Username     *string `json:"username"`
	Password     *string `json:"password"`
	SSLMode      *string `json:"sslmode"`
}

var hostname = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

func normalize(in Input) (Input, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Host), "."))
	// Preserve DNS names for TLS hostname verification. The unique index handles
	// localhost/127.0.0.1 equivalence without changing the connection hostname.
	if ip := net.ParseIP(in.Host); ip != nil {
		in.Host = ip.String()
	} else if len(in.Host) > 253 || !hostname.MatchString(in.Host) {
		return Input{}, ErrInvalid
	}
	if in.Port == 0 {
		in.Port = 5432
	}
	if in.SSLMode == "" {
		in.SSLMode = "verify-full"
	}
	if in.Port < 1 || in.Port > 65535 || len(in.Name) < 1 || len(in.Name) > 100 || len(in.DatabaseName) < 1 || len(in.DatabaseName) > 63 || len(in.Username) < 1 || len(in.Username) > 63 {
		return Input{}, ErrInvalid
	}
	if strings.ContainsAny(in.Name+in.DatabaseName+in.Username, "\x00") {
		return Input{}, ErrInvalid
	}
	if in.SSLMode != "disable" && in.SSLMode != "require" && in.SSLMode != "verify-ca" && in.SSLMode != "verify-full" {
		return Input{}, ErrInvalid
	}
	if in.Password != nil && (len(*in.Password) > 4096 || strings.ContainsRune(*in.Password, 0)) {
		return Input{}, ErrInvalid
	}
	return in, nil
}

type Service struct {
	db  *pgxpool.Pool
	box *secretbox.Box
}

func New(db *pgxpool.Pool, key []byte) (*Service, error) {
	box, err := secretbox.New(key)
	if err != nil {
		return nil, err
	}
	return &Service{db: db, box: box}, nil
}

func requireAdmin(p auth.Principal) error {
	if p.ID < 1 {
		return auth.ErrUnauthenticated
	}
	if !p.IsPlatformAdmin {
		return ErrForbidden
	}
	return nil
}

func databaseError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23503") {
		return ErrConflict
	}
	return err
}

const columns = "id, name, host, port, database_name, username, sslmode, created_at, password_encrypted"

func scan(row pgx.Row) (Connection, error) {
	var c Connection
	err := row.Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.DatabaseName, &c.Username, &c.SSLMode, &c.CreatedAt, &c.passwordEncrypted)
	return c, databaseError(err)
}

func (s *Service) Create(ctx context.Context, p auth.Principal, input Input) (Connection, error) {
	if err := requireAdmin(p); err != nil {
		return Connection{}, err
	}
	in, err := normalize(input)
	if err != nil {
		return Connection{}, err
	}
	if in.Password == nil || *in.Password == "" {
		return Connection{}, ErrInvalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(context.Background())

	c, err := scan(tx.QueryRow(ctx, `
		INSERT INTO database_connections (name, host, port, database_name, username, password_encrypted, sslmode)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+columns,
		in.Name, in.Host, in.Port, in.DatabaseName, in.Username, []byte{}, in.SSLMode,
	))
	if err != nil {
		return Connection{}, err
	}
	encrypted := s.box.Seal([]byte(*in.Password), fmt.Sprintf("target:%d", c.ID))
	if _, err = tx.Exec(ctx, "UPDATE database_connections SET password_encrypted = $2 WHERE id = $1", c.ID, encrypted); err != nil {
		return Connection{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Connection{}, err
	}
	return c, nil
}

func (s *Service) List(ctx context.Context, p auth.Principal, admin bool) ([]Connection, error) {
	if p.ID < 1 {
		return nil, auth.ErrUnauthenticated
	}
	if admin {
		if err := requireAdmin(p); err != nil {
			return nil, err
		}
	}

	query := "SELECT " + columns + ", ''::text FROM database_connections ORDER BY id"
	var args []any
	if !admin {
		query = `
			SELECT
				d.id, d.name, d.host, d.port, d.database_name, d.username, d.sslmode, d.created_at, d.password_encrypted,
				CASE WHEN bool_or(p.access_level = 'edit') THEN 'edit' ELSE 'view' END
			FROM database_connections d
			JOIN group_database_permissions p ON p.database_id = d.id
			JOIN user_groups ug ON ug.group_id = p.group_id
			JOIN users u ON u.id = ug.user_id
			WHERE ug.user_id = $1 AND u.is_active = true
			GROUP BY d.id
			ORDER BY d.id
		`
		args = []any{p.ID}
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]Connection, 0)
	for rows.Next() {
		var c Connection
		err = rows.Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.DatabaseName, &c.Username, &c.SSLMode, &c.CreatedAt, &c.passwordEncrypted, &c.Access)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Service) Update(ctx context.Context, p auth.Principal, id int64, patch Patch) (Connection, error) {
	if err := requireAdmin(p); err != nil {
		return Connection{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(context.Background())

	c, err := scan(tx.QueryRow(ctx, "SELECT "+columns+" FROM database_connections WHERE id = $1 FOR UPDATE", id))
	if err != nil {
		return Connection{}, err
	}

	in := Input{
		Name:         c.Name,
		Host:         c.Host,
		Port:         c.Port,
		DatabaseName: c.DatabaseName,
		Username:     c.Username,
		SSLMode:      c.SSLMode,
		Password:     patch.Password,
	}
	if patch.Name != nil {
		in.Name = *patch.Name
	}
	if patch.Host != nil {
		in.Host = *patch.Host
	}
	if patch.Port != nil {
		if *patch.Port == 0 {
			return Connection{}, ErrInvalid
		}
		in.Port = *patch.Port
	}
	if patch.DatabaseName != nil {
		in.DatabaseName = *patch.DatabaseName
	}
	if patch.Username != nil {
		in.Username = *patch.Username
	}
	if patch.SSLMode != nil {
		in.SSLMode = *patch.SSLMode
	}

	in, err = normalize(in)
	if err != nil {
		return Connection{}, err
	}
	if in.Host != c.Host || in.Port != c.Port || in.DatabaseName != c.DatabaseName {
		return Connection{}, ErrConflict
	}

	encrypted := c.passwordEncrypted
	if in.Password != nil && *in.Password != "" {
		encrypted = s.box.Seal([]byte(*in.Password), fmt.Sprintf("target:%d", id))
	}

	c, err = scan(tx.QueryRow(ctx, `
		UPDATE database_connections
		SET name = $2, username = $3, password_encrypted = $4, sslmode = $5
		WHERE id = $1
		RETURNING `+columns,
		id, in.Name, in.Username, encrypted, in.SSLMode,
	))
	if err != nil {
		return Connection{}, err
	}
	return c, tx.Commit(ctx)
}

func (s *Service) Delete(ctx context.Context, p auth.Principal, id int64) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())

	if _, err = scan(tx.QueryRow(ctx, "SELECT "+columns+" FROM database_connections WHERE id = $1 FOR UPDATE", id)); err != nil {
		return err
	}
	// Deleting the grant-bearing connection revokes every affected session.
	_, err = tx.Exec(ctx, `
		UPDATE sessions
		SET revoked_at = now()
		WHERE revoked_at IS NULL
		  AND user_id IN (
			SELECT ug.user_id
			FROM user_groups ug
			JOIN group_database_permissions p ON p.group_id = ug.group_id
			WHERE p.database_id = $1
		  )
	`, id)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM database_connections WHERE id = $1", id); err != nil {
		return databaseError(err)
	}
	return tx.Commit(ctx)
}

func (s *Service) Test(ctx context.Context, p auth.Principal, id int64) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	c, err := scan(s.db.QueryRow(ctx, "SELECT "+columns+" FROM database_connections WHERE id = $1", id))
	if err != nil {
		return err
	}
	pool, err := s.open(ctx, c)
	if err != nil {
		return err
	}
	pool.Close()
	return nil
}

// OpenTarget checks RBAC against the metadata DB, decrypts the target credential,
// and opens a request-scoped connection pool to the target PostgreSQL database.
func (s *Service) OpenTarget(ctx context.Context, p auth.Principal, id int64, write bool) (*pgxpool.Pool, error) {
	if p.ID < 1 {
		return nil, auth.ErrUnauthenticated
	}
	access, err := policy.DatabaseAccess(ctx, s.db, p.ID, id)
	if err != nil {
		return nil, err
	}
	if !policy.Allows(access, write) {
		return nil, ErrForbidden
	}
	c, err := scan(s.db.QueryRow(ctx, "SELECT "+columns+" FROM database_connections WHERE id = $1", id))
	if err != nil {
		return nil, err
	}
	return s.open(ctx, c)
}

func (s *Service) open(ctx context.Context, c Connection) (*pgxpool.Pool, error) {
	password, err := s.box.Open(c.passwordEncrypted, fmt.Sprintf("target:%d", c.ID))
	if err != nil {
		return nil, errors.New("cannot decrypt target credential")
	}

	u := url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(c.Host, fmt.Sprint(c.Port)),
		Path:     "/" + c.DatabaseName,
		User:     url.UserPassword(c.Username, string(password)),
		RawQuery: url.Values{"sslmode": {c.SSLMode}, "connect_timeout": {"5"}}.Encode(),
	}
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, ErrTargetUnavailable
	}
	cfg.MaxConns = 3
	cfg.ConnConfig.RuntimeParams = map[string]string{"application_name": "terserah-target"}

	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, cfg)
	if err != nil {
		return nil, ErrTargetUnavailable
	}
	if err = pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, ErrTargetUnavailable
	}

	// Compare the actual database name and postmaster start time so the metadata
	// database cannot be registered as a target even through host/DNS aliases.
	var metadataName, targetName string
	var metadataStart, targetStart time.Time
	const instanceQuery = "SELECT pg_catalog.current_database(), pg_catalog.pg_postmaster_start_time()"
	if err = s.db.QueryRow(connectCtx, instanceQuery).Scan(&metadataName, &metadataStart); err != nil {
		pool.Close()
		return nil, errors.New("cannot inspect metadata database")
	}
	if err = pool.QueryRow(connectCtx, instanceQuery).Scan(&targetName, &targetStart); err != nil {
		pool.Close()
		return nil, ErrTargetUnavailable
	}
	if targetName == metadataName && targetStart.Equal(metadataStart) {
		pool.Close()
		return nil, ErrMetadataTarget
	}
	return pool, nil
}
