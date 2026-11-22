package batch

import (
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
)

const DownPrefix = "-- terserah-compensation-v1 "

// DownSQL is frozen in the definition, then approved as the SQL for compensation.
// $1 contains only verified recovery images for one committed batch.
func DownSQL(c Config) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	sets := []string{}
	for _, name := range c.Columns() {
		column := pgx.Identifier{name}.Sanitize()
		sets = append(sets, column+" = (saved.row_before)."+column)
	}
	key := pgx.Identifier{c.Key}.Sanitize()
	data, err := json.Marshal(c)
	if err != nil {
		return "", ErrInvalid
	}
	sql := DownPrefix + string(data) + "\nUPDATE " + c.Relation() + " AS target SET " + strings.Join(sets, ", ") + `
FROM (SELECT pg_catalog.jsonb_populate_record(NULL::` + c.Relation() + `, image->'before') AS row_before,
 image->'after' AS row_after FROM pg_catalog.jsonb_array_elements($1::jsonb) AS image) AS saved
WHERE target.` + key + ` = (saved.row_before).` + key + ` AND pg_catalog.to_jsonb(target) = saved.row_after
RETURNING target.` + key + `, CASE WHEN pg_catalog.octet_length(pg_catalog.to_jsonb(target)::text)<=8388608 THEN pg_catalog.to_jsonb(target) ELSE NULL END`
	if len(sql) > 48*1024 {
		return "", ErrInvalid
	}
	return sql, nil
}

func ParseDown(sql string) (*Config, error) {
	trimmed := strings.TrimSpace(sql)
	if !strings.HasPrefix(trimmed, "-- terserah-compensation") {
		return nil, nil
	}
	if !strings.HasPrefix(trimmed, DownPrefix) || len(sql) > 48*1024 {
		return nil, ErrInvalid
	}
	header, _, ok := strings.Cut(trimmed, "\n")
	if !ok {
		return nil, ErrInvalid
	}
	var c Config
	if err := json.Unmarshal([]byte(strings.TrimPrefix(header, DownPrefix)), &c); err != nil {
		return nil, ErrInvalid
	}
	canonical, err := DownSQL(c)
	if err != nil || canonical != trimmed {
		return nil, ErrInvalid
	}
	return &c, nil
}

type CompensationManifest struct {
	ExecutionID, SourceExecutionID, SourceSignature, SQL string
	DatabaseID, Version, ActorID, Batches, Rows          int64
}

func CompensationSignature(key []byte, m CompensationManifest) string {
	data, _ := json.Marshal([]any{"terserah-compensation-manifest-v1", m})
	return signature(key, data)
}
