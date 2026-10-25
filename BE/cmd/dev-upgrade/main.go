// dev-upgrade splits the original local demo executor without resetting data.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/config"
	"terserah/backend/internal/database"
	"terserah/backend/internal/secretbox"
)

type executor struct {
	Database string `json:"database"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

func main() {
	root := flag.String("root", "..", "repository root containing tmp/dev")
	flag.Parse()
	if err := run(*root); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(root string) error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return errors.New("load the existing .env before upgrading; keep its encryption key")
	}
	adminCfg, err := pgx.ParseConfig(cfg.DatabaseURL)
	if err != nil || adminCfg.Database != "wbd_dev_platform" || adminCfg.User != "wbd_dev_meta" {
		return errors.New("this upgrade only supports the original wbd_dev local demo databases")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	metadata, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("cannot open development metadata")
	}
	defer metadata.Close()
	box, _ := secretbox.New(cfg.SecretKey)
	rows, err := metadata.Query(ctx, `SELECT id,password_encrypted FROM database_connections WHERE database_name IN ('wbd_dev_target_a','wbd_dev_target_b')`)
	if err != nil {
		return errors.New("cannot inspect existing credentials")
	}
	for rows.Next() {
		var id int64
		var encrypted []byte
		if err = rows.Scan(&id, &encrypted); err == nil {
			_, err = box.Open(encrypted, fmt.Sprintf("target:%d", id))
		}
		if err != nil {
			rows.Close()
			return errors.New("cannot decrypt existing credentials; restore the original encryption key before upgrading")
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return errors.New("cannot inspect existing credentials")
	}
	var metadataPID uint32
	if err = metadata.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&metadataPID); err != nil {
		return errors.New("cannot inspect metadata connection")
	}
	adminCfg.User = os.Getenv("DEV_PG_ADMIN_USER")
	if adminCfg.User == "" {
		adminCfg.User = "postgres"
	}
	adminCfg.Password = os.Getenv("DEV_PG_ADMIN_PASSWORD")
	adminCfg.Database = "postgres"
	adminCfg.RuntimeParams = map[string]string{"search_path": "pg_catalog,pg_temp"}
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		return errors.New("cannot authenticate PostgreSQL administrator")
	}
	defer admin.Close(context.Background())
	if _, err = admin.Exec(ctx, "SELECT pg_advisory_lock(3110004)"); err != nil {
		return errors.New("cannot lock development upgrade")
	}
	var busy, legacy bool
	err = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename IN ('wbd_dev_meta','wbd_dev_exec') AND pid<>$1), EXISTS(SELECT 1 FROM pg_roles WHERE rolname='wbd_dev_exec')`, metadataPID).Scan(&busy, &legacy)
	if err != nil || busy {
		return errors.New("stop FE/BE and other development database clients before upgrading")
	}
	if !legacy {
		slog.Info("no legacy shared executor; no changes needed")
		return nil
	}
	// Never reassign objects belonging to an unrelated database/project.
	var safe bool
	err = admin.QueryRow(ctx, `SELECT
 (SELECT count(*)=2 FROM pg_database d JOIN pg_roles r ON r.oid=d.datdba WHERE d.datname IN ('wbd_dev_target_a','wbd_dev_target_b') AND r.rolname='wbd_dev_owner')
 AND NOT EXISTS(SELECT 1 FROM pg_shdepend dep WHERE dep.refclassid='pg_authid'::regclass AND dep.refobjid=(SELECT oid FROM pg_roles WHERE rolname='wbd_dev_exec')
 AND ((dep.dbid<>0 AND dep.dbid NOT IN (SELECT oid FROM pg_database WHERE datname IN ('wbd_dev_target_a','wbd_dev_target_b')))
 OR (dep.classid='pg_database'::regclass AND dep.objid NOT IN (SELECT oid FROM pg_database WHERE datname IN ('wbd_dev_target_a','wbd_dev_target_b')))))`).Scan(&safe)
	if err != nil || !safe {
		return errors.New("legacy executor has unexpected dependencies or target ownership; manual administrator review required")
	}
	directory := filepath.Join(root, "tmp", "dev")
	if err = os.MkdirAll(directory, 0700); err != nil {
		return errors.New("cannot create private upgrade directory")
	}
	plan, err := loadPlan(ctx, admin, filepath.Join(directory, "executor-upgrade.json"))
	if err != nil {
		return err
	}
	// NOLOGIN prevents new old-role sessions while ownership is being transferred.
	if _, err = admin.Exec(ctx, "ALTER ROLE wbd_dev_exec NOLOGIN"); err != nil {
		return errors.New("cannot disable legacy login; no ownership transferred")
	}
	for _, target := range plan {
		var exists bool
		if err = admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", target.Role).Scan(&exists); err != nil {
			return errors.New("cannot inspect upgrade role")
		}
		if !exists {
			var command string
			if err = admin.QueryRow(ctx, "SELECT format('CREATE ROLE %I LOGIN PASSWORD %L',$1::text,$2::text)", target.Role, target.Password).Scan(&command); err != nil {
				return errors.New("cannot prepare dedicated executor")
			}
			if _, err = admin.Exec(ctx, command); err != nil {
				return errors.New("cannot create dedicated executor; rerun upgrade after resolving the cause")
			}
		}
		targetCfg := adminCfg.Copy()
		targetCfg.Database = target.Database
		conn, e := pgx.ConnectConfig(ctx, targetCfg)
		if e != nil {
			return errors.New("cannot open upgrade target; credentials are saved for retry")
		}
		e = upgradeTarget(ctx, conn, target)
		conn.Close(context.Background())
		if e != nil {
			return errors.New("target upgrade incomplete; rerun upgrade with the saved credentials")
		}
	}
	tx, err := metadata.Begin(ctx)
	if err != nil {
		return errors.New("cannot start metadata credential update; rerun upgrade")
	}
	defer tx.Rollback(context.Background())
	for _, target := range plan {
		rows, e := tx.Query(ctx, `SELECT id FROM database_connections WHERE database_name=$1 AND port=$2 AND host IN ('127.0.0.1','localhost','::1',$3) AND username IN ('wbd_dev_exec',$4) FOR UPDATE`, target.Database, int(adminCfg.Port), adminCfg.Host, target.Role)
		if e != nil {
			return errors.New("cannot inspect configured demo targets")
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if e = rows.Scan(&id); e != nil {
				break
			}
			ids = append(ids, id)
		}
		rows.Close()
		if e != nil || rows.Err() != nil {
			return errors.New("cannot read configured demo targets")
		}
		for _, id := range ids {
			if _, e = tx.Exec(ctx, `UPDATE database_connections SET username=$2,password_encrypted=$3 WHERE id=$1`, id, target.Role, box.Seal([]byte(target.Password), fmt.Sprintf("target:%d", id))); e != nil {
				return errors.New("cannot update encrypted credentials; rerun upgrade")
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("cannot commit credential update; rerun upgrade")
	}
	path := filepath.Join(directory, "database-credentials.txt")
	previous, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot read local credential file; executor-upgrade.json retains recovery credentials")
	}
	prefix := strings.SplitN(string(previous), "Add these two targets", 2)[0]
	text := prefix + "Add these two targets through Connections:\n"
	for _, target := range plan {
		text += fmt.Sprintf("\nDatabase: %s\nHost: %s\nPort: %d\nUsername: %s\nPassword: %s\nSSL mode: disable (local development)\n", target.Database, adminCfg.Host, adminCfg.Port, target.Role, target.Password)
	}
	if err = os.WriteFile(path, []byte(text), 0600); err != nil {
		return errors.New("cannot write credential guidance; rerun upgrade")
	}
	slog.Info("dedicated executors installed; data, connection IDs, history, target identities and encryption key preserved")
	return nil
}

func loadPlan(ctx context.Context, admin *pgx.Conn, path string) ([]executor, error) {
	plan := []executor{{Database: "wbd_dev_target_a", Role: "wbd_dev_exec_a"}, {Database: "wbd_dev_target_b", Role: "wbd_dev_exec_b"}}
	data, err := os.ReadFile(path)
	if err == nil {
		var saved []executor
		if json.Unmarshal(data, &saved) != nil || len(saved) != len(plan) {
			return nil, errors.New("invalid saved upgrade plan; do not regenerate credentials")
		}
		for i, target := range saved {
			password, e := base64.StdEncoding.DecodeString(target.Password)
			if target.Database != plan[i].Database || target.Role != plan[i].Role || e != nil || len(password) != 32 {
				return nil, errors.New("invalid saved upgrade plan; do not regenerate credentials")
			}
		}
		return saved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot read saved upgrade plan")
	}
	for i := range plan {
		var exists bool
		if err = admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", plan[i].Role).Scan(&exists); err != nil || exists {
			return nil, errors.New("dedicated role already exists without an upgrade plan; refusing to overwrite credentials")
		}
		password := make([]byte, 32)
		if _, err = rand.Read(password); err != nil {
			return nil, errors.New("cannot generate executor password")
		}
		plan[i].Password = base64.StdEncoding.EncodeToString(password)
	}
	data, _ = json.MarshalIndent(plan, "", "  ")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("cannot persist upgrade plan")
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return nil, errors.New("cannot persist complete upgrade plan; no roles changed")
	}
	return plan, nil
}

func upgradeTarget(ctx context.Context, conn *pgx.Conn, target executor) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(3110003)"); err != nil {
		return err
	}
	role, db := pgx.Identifier{target.Role}.Sanitize(), pgx.Identifier{target.Database}.Sanitize()
	_, err = tx.Exec(ctx, `REASSIGN OWNED BY wbd_dev_exec TO `+role+`;
REVOKE ALL ON DATABASE `+db+` FROM wbd_dev_exec;
GRANT CONNECT, CREATE ON DATABASE `+db+` TO `+role+`;
REVOKE ALL ON SCHEMA public, _terserah FROM wbd_dev_exec;
REVOKE ALL ON _terserah.migration_receipts, _terserah.target_identity FROM wbd_dev_exec;
GRANT USAGE, CREATE ON SCHEMA public TO `+role+`;
GRANT USAGE ON SCHEMA _terserah TO `+role+`;
GRANT SELECT, INSERT ON _terserah.migration_receipts TO `+role+`;
GRANT SELECT ON _terserah.target_identity TO `+role+`;`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
