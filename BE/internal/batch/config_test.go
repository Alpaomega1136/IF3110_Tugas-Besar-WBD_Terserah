package batch

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBatchSnapshotBindsConfigurationAndParameterizedSQL(t *testing.T) {
	c := Config{Schema: "odd schema", Table: `items"; DROP TABLE users;--`, Key: "id", Size: 250, Set: map[string]json.RawMessage{"amount": json.RawMessage("9007199254740993.125"), "note": json.RawMessage(`"O'Reilly"`)}}
	sql, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(sql)
	if err != nil || parsed.Size != 250 || string(parsed.Set["amount"]) != "9007199254740993.125" {
		t.Fatal("batch snapshot lost configuration or precision", err)
	}
	if !strings.Contains(c.Query(), `"items""; DROP TABLE users;--"`) || strings.Contains(c.Query(), "O'Reilly") || !strings.Contains(c.Query(), "$1::bigint[]") {
		t.Fatal("identifiers or replacement values are not isolated from SQL")
	}
	for _, altered := range []string{sql + "; DELETE FROM users", strings.Replace(sql, `"size":250`, `"size":5001`, 1), strings.Replace(sql, "$1::bigint[]", "ARRAY[1]", 1), "-- terserah-batch-v9 {}\nUPDATE t SET x=1"} {
		if _, err := Parse(altered); err == nil {
			t.Fatal("batch snapshot tampering accepted")
		}
	}
	if c, err := Parse("UPDATE t SET x=1"); c != nil || err != nil {
		t.Fatal("ordinary SQL classified as batch")
	}
}

func TestBatchConfigurationBoundaries(t *testing.T) {
	valid := func() Config {
		return Config{Schema: "public", Table: "items", Key: "id", Size: 1, Set: map[string]json.RawMessage{"flag": json.RawMessage("true")}}
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Size = 0 }, func(c *Config) { c.Size = 5001 }, func(c *Config) { c.Key = "" }, func(c *Config) { c.Schema = "_terserah" }, func(c *Config) { c.Schema = "pg_temp" }, func(c *Config) { c.Table = "bad\x00name" }, func(c *Config) { c.Set["id"] = json.RawMessage("1") }, func(c *Config) { c.Set["flag"] = json.RawMessage("broken") }, func(c *Config) { c.Set = nil }} {
		c := valid()
		mutate(&c)
		if _, err := Encode(c); err == nil {
			t.Fatal("unsafe batch configuration accepted")
		}
	}
	c := valid()
	c.Size = 5000
	c.Set["flag"] = json.RawMessage("null")
	if _, err := Encode(c); err != nil {
		t.Fatal("valid boundary rejected", err)
	}
}
