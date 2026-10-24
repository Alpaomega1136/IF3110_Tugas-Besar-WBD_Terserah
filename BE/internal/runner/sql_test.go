package runner

import (
	"strings"
	"testing"
)

func TestStatements(t *testing.T) {
	for _, sql := range []string{
		`CREATE TABLE test(id int); INSERT INTO test VALUES(1)`,
		`SELECT ';COMMIT;', 'it''s;fine', "semi;colon"; SELECT 2`,
		`SELECT E'escaped\';COMMIT;'; SELECT 2`,
		"/* outer /* nested ; COMMIT */ ok */ SELECT 1; -- ;ROLLBACK\r\nSELECT 2",
		`CREATE FUNCTION f() RETURNS text AS $body$ BEGIN RETURN ';COMMIT;'; END; $body$ LANGUAGE plpgsql; SELECT 2`,
		`SELECT $$semicolon;$$; SELECT 2`,
	} {
		parts, err := statements(sql)
		if err != nil || len(parts) != 2 {
			t.Errorf("SQL=%q parts=%v err=%v", sql, parts, err)
		}
	}
	for _, sql := range []string{"", "-- only comment", ";/*nothing*/;", "SELECT 'unclosed", "SELECT $$unclosed", "SELECT 1 /* unclosed", "BEGIN; SELECT 1", "SELECT 1; COMMIT", "ROLLBACK", "END", "ABORT", "START TRANSACTION", "SAVEPOINT a", "RELEASE a", "PREPARE TRANSACTION 'a'", "SET statement_timeout=0", "DO $$BEGIN COMMIT; END$$", "CALL p()", "COPY t FROM STDIN", "VACUUM", "ALTER SYSTEM SET x=1", "CREATE TABLESPACE x", "CREATE INDEX CONCURRENTLY x ON t(id)", "CREATE DATABASE x", "SELECT 1\x00", strings.Repeat("SELECT 1;", 101), strings.Repeat(" ", 48*1024+1)} {
		if _, err := statements(sql); err == nil {
			t.Errorf("accepted %q", sql)
		}
	}
}

func FuzzStatements(f *testing.F) {
	for _, sql := range []string{"SELECT 1;COMMIT", `SELECT E'\';COMMIT;'`, "/* nested /* */ */ SELECT $$;$$", "SELECT \xff"} {
		f.Add(sql)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		parts, err := statements(sql)
		if err == nil && (len(parts) < 1 || len(parts) > 100) {
			t.Fatal("invalid statement count")
		}
	})
}
