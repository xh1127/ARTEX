package evidence

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// suiteLockKey is the advisory lock the db, agent, server and llmrec suites hold
// for their whole run, so only one of them touches the shared database at a time.
const suiteLockKey = 7337741002

func TestMain(m *testing.M) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		os.Exit(m.Run())
	}
	os.Exit(runEvidenceSuite(m))
}

// runEvidenceSuite takes the suite lock FIRST and only then opens the database.
//
// db.Open re-runs the schema DDL. This used to happen before the lock was taken
// ("initialize a fresh database, then lock"), i.e. concurrently with whichever
// suite held the lock: the DDL and that suite's statements formed lock cycles,
// and PostgreSQL broke them by aborting one side — db.Open retries its own, the
// other suite's test simply failed with "deadlock detected".
//
// The one case that cannot lock first is a database that does not exist yet:
// there is nothing to connect to. Only then is db.Open (which creates it) run
// before the lock.
func runEvidenceSuite(m *testing.M) int {
	dsn := os.Getenv("ARTEX_PG_DSN")
	unlock, err := lockSuite(dsn)
	if err != nil {
		pg, openErr := db.Open(dsn) // creates the database when it is missing
		if openErr != nil {
			panic(openErr)
		}
		pg.Close()
		if unlock, err = lockSuite(dsn); err != nil {
			panic(err)
		}
	}
	defer unlock()
	pg, err := db.Open(dsn) // schema in place for the tests — now under the lock
	if err != nil {
		panic(err)
	}
	defer pg.Close()
	return m.Run()
}

// lockSuite takes the suite lock on one pinned connection (a session-level lock
// belongs to the session, so it must not go through a pool).
func lockSuite(dsn string) (unlock func(), err error) {
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, int64(suiteLockKey)); err != nil {
		conn.Close()
		pool.Close()
		return nil, err
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(suiteLockKey))
		conn.Close()
		pool.Close()
	}, nil
}
