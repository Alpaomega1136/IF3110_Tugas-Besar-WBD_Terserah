package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSavedUpgradePlanReusesCredentials(t *testing.T) {
	password := base64.StdEncoding.EncodeToString(make([]byte, 32))
	saved := []executor{{"wbd_dev_target_a", "wbd_dev_exec_a", password}, {"wbd_dev_target_b", "wbd_dev_exec_b", password}}
	path := filepath.Join(t.TempDir(), "plan.json")
	write := func(plan []executor) {
		t.Helper()
		data, _ := json.Marshal(plan)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(saved)
	plan, err := loadPlan(context.Background(), nil, path)
	if err != nil || plan[0].Password != password || plan[1].Password != password {
		t.Fatal("retry replaced saved executor credentials")
	}
	saved[0].Database = "unrelated_database"
	write(saved)
	if _, err = loadPlan(context.Background(), nil, path); err == nil {
		t.Fatal("upgrade accepted an unrelated database")
	}
	saved[0].Database = "wbd_dev_target_a"
	saved[1].Password = "invalid"
	write(saved)
	if _, err = loadPlan(context.Background(), nil, path); err == nil {
		t.Fatal("invalid saved credentials silently replaced")
	}
}
