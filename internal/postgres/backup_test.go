package postgres

import (
	"strings"
	"testing"

	"pm/internal/pmdir"
)

// RestoreFile validates the target database name before touching the
// filesystem or shelling out to pg_restore — these checks must reject
// bad input without a live server.
func TestRestoreFileRejectsBadNames(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	cases := []string{"", "bad name", "postgres", "template0", "'; drop--"}
	for _, name := range cases {
		if err := RestoreFile(paths, "17.0", "does-not-matter.dump", name); err == nil {
			t.Errorf("RestoreFile(%q) = nil error, want one", name)
		}
	}
}

func TestPgClientEnvIncludesPassword(t *testing.T) {
	env := pgClientEnv("se'cret")
	found := false
	for _, kv := range env {
		if kv == "PGPASSWORD=se'cret" {
			found = true
		}
		if strings.HasPrefix(strings.ToUpper(kv), "PG") && !strings.HasPrefix(kv, "PGPASSWORD") && !strings.HasPrefix(kv, "PGCONNECT_TIMEOUT") {
			t.Errorf("pgClientEnv leaked a foreign PG* variable: %s", kv)
		}
	}
	if !found {
		t.Fatal("PGPASSWORD not set when a password is given")
	}
	if env2 := pgClientEnv(""); containsPrefix(env2, "PGPASSWORD=") {
		t.Fatal("PGPASSWORD should be absent when no password is set")
	}
}

func containsPrefix(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}
