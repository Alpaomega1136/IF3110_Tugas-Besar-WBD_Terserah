// Package batch defines the supported, approval-bound batch UPDATE format.
package batch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const Prefix = "-- terserah-batch-v1 "
const MaxBytes = 8 * 1024 * 1024

var ErrInvalid = errors.New("invalid batch configuration or SQL snapshot")

type Config struct {
	Schema string                     `json:"schema"`
	Table  string                     `json:"table"`
	Key    string                     `json:"key"`
	Size   int                        `json:"size"`
	Set    map[string]json.RawMessage `json:"set"`
}

func nameValid(s string) bool {
	return len(s) > 0 && len(s) <= 63 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func (c Config) Validate() error {
	if !nameValid(c.Schema) || !nameValid(c.Table) || !nameValid(c.Key) || c.Size < 1 || c.Size > 5000 || len(c.Set) < 1 || len(c.Set) > 16 ||
		strings.HasPrefix(c.Schema, "pg_") || c.Schema == "information_schema" || c.Schema == "_terserah" {
		return ErrInvalid
	}
	for name, value := range c.Set {
		if !nameValid(name) || name == c.Key || !json.Valid(value) {
			return ErrInvalid
		}
	}
	return nil
}

func (c Config) Relation() string { return pgx.Identifier{c.Schema, c.Table}.Sanitize() }
func (c Config) Columns() []string {
	columns := make([]string, 0, len(c.Set))
	for name := range c.Set {
		columns = append(columns, name)
	}
	sort.Strings(columns)
	return columns
}
func (c Config) Values() []byte { data, _ := json.Marshal(c.Set); return data }

// Query binds only selected keys and typed replacement values, never SQL fragments.
func (c Config) Query() string {
	sets := []string{}
	for _, name := range c.Columns() {
		column := pgx.Identifier{name}.Sanitize()
		sets = append(sets, column+" = patch."+column)
	}
	return "UPDATE " + c.Relation() + " AS target SET " + strings.Join(sets, ", ") + "\nFROM pg_catalog.jsonb_populate_record(NULL::" + c.Relation() + ", $2::jsonb) AS patch\nWHERE target." + pgx.Identifier{c.Key}.Sanitize() + " = ANY($1::bigint[])\nRETURNING target." + pgx.Identifier{c.Key}.Sanitize() + ", CASE WHEN pg_catalog.octet_length(pg_catalog.to_jsonb(target)::text)<=8388608 THEN pg_catalog.to_jsonb(target) ELSE NULL END"
}

func Encode(c Config) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", ErrInvalid
	}
	sql := Prefix + string(data) + "\n" + c.Query()
	if len(sql) > 48*1024 {
		return "", ErrInvalid
	}
	return sql, nil
}

func Parse(sql string) (*Config, error) {
	trimmed := strings.TrimSpace(sql)
	if !strings.HasPrefix(trimmed, "-- terserah-batch") {
		return nil, nil
	}
	if !strings.HasPrefix(trimmed, Prefix) || len(sql) > 48*1024 {
		return nil, ErrInvalid
	}
	header, _, ok := strings.Cut(trimmed, "\n")
	if !ok {
		return nil, ErrInvalid
	}
	var c Config
	decoder := json.NewDecoder(bytes.NewBufferString(strings.TrimPrefix(header, Prefix)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return nil, ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, ErrInvalid
	}
	canonical, err := Encode(c)
	if err != nil || canonical != trimmed {
		return nil, ErrInvalid
	}
	return &c, nil
}
