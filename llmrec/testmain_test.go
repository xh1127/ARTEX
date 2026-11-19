package llmrec

import (
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain acquires the same PostgreSQL advisory lock (7337741002) as the db,
// agent, server and evidence suites. This package has one database-backed test,
// and db.Open re-runs the schema DDL: without the lock that DDL ran concurrently
// with whichever of those suites held it, and PostgreSQL resolved the resulting
// lock cycle by aborting one side — sometimes the other package's query, failing
// an unrelated test with "deadlock detected".
func TestMain(m *testing.M) {
	dsn, _, err := db.DSN()
	if err != nil {
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	os.Exit(m.Run())
}
