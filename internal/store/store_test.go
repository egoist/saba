package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenMovesTheDatabaseBackupPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"schema_version":1,"server":{},"apps":{"main":{"name":"main","kind":"database",
		"database":{"engine":"postgres","version":"17","env_name":"DATABASE_URL","backup":{"schedule":"@daily","keep":14}}}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Snapshot().Apps["main"]
	if a.Backup == nil || a.Backup.Schedule != "@daily" || a.Backup.Keep != 14 || a.Database.LegacyBackup != nil {
		t.Fatalf("policy not migrated: %+v %+v", a.Backup, a.Database)
	}
}
