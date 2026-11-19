package server

import (
	"testing"

	"github.com/Autumn-27/artex/config"
)

// skipWithoutPG skips a database-backed test when no database is configured at
// all (neither ARTEX_PG_DSN nor a config.json database). A configured database
// that fails to open still fails the test: skipping on any NewManager error
// would report a broken CI database as a green, fully skipped suite.
func skipWithoutPG(t *testing.T) {
	t.Helper()
	if _, _, err := config.PostgresDSN(); err != nil {
		t.Skipf("postgres not configured - skipping (%v)", err)
	}
}
