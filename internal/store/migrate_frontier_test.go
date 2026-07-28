package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// matrixBackend is one length-enforcing SQL backend the portable migration and
// dedup tests exercise. sqlite is deliberately excluded here: it treats
// VARCHAR(n) as unlimited TEXT, so it cannot reproduce the varchar(512) ->
// varchar(64) narrowing failure these tests guard against.
type matrixBackend struct{ driver, dsn string }

// matrixBackends returns the mysql/postgres backends configured via
// ARDVARK_TEST_MYSQL_DSN / ARDVARK_TEST_POSTGRES_DSN, and skips the test when
// neither is set. These tests must run against a real length-enforcing engine
// to be meaningful — sqlite would pass even against the old buggy raw-key code
// — so they are env-gated rather than baked into the default `go test` run
// (the smoketest harness in tools/smoketest wires up matching containers on
// ports 13306/13307).
func matrixBackends(t *testing.T) []matrixBackend {
	t.Helper()
	var out []matrixBackend
	if dsn := os.Getenv("ARDVARK_TEST_MYSQL_DSN"); dsn != "" {
		out = append(out, matrixBackend{"mysql", dsn})
	}
	if dsn := os.Getenv("ARDVARK_TEST_POSTGRES_DSN"); dsn != "" {
		out = append(out, matrixBackend{"postgres", dsn})
	}
	if len(out) == 0 {
		t.Skip("set ARDVARK_TEST_MYSQL_DSN and/or ARDVARK_TEST_POSTGRES_DSN to run length-enforcing backend tests")
	}
	return out
}

func openRawMatrix(t *testing.T, b matrixBackend) *gorm.DB {
	t.Helper()
	var d gorm.Dialector
	switch b.driver {
	case "mysql":
		d = mysql.Open(b.dsn)
	case "postgres":
		d = postgres.Open(b.dsn)
	default:
		t.Fatalf("openRawMatrix: unsupported driver %q", b.driver)
	}
	db, err := gorm.Open(d, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("opening raw %s db: %v", b.driver, err)
	}
	return db
}

// legacyFrontierItem models the <=0.4.0 frontier_items.dedup_key column: the
// raw "kind:natural" string in a varchar(512). It maps to the same table name
// as store.FrontierItem so that seeding this shape and then calling store.Open
// reproduces the real 0.4.0 -> 0.4.1 upgrade path.
type legacyFrontierItem struct {
	ID       uint   `gorm:"primarykey"`
	DedupKey string `gorm:"uniqueIndex;size:512"`
}

func (legacyFrontierItem) TableName() string { return "frontier_items" }

// TestOpen_MigratesLegacyWideDedupKey reproduces the primary v0.4.1 upgrade
// path against a real length-enforcing backend: a populated 0.4.0 frontier
// whose dedup_key is varchar(512) holding a >64-char raw key. Without the
// pre-migration reconcile, AutoMigrate's varchar(512) -> varchar(64) ALTER
// hard-fails ("Data truncated" on mysql strict mode, 22001 on postgres) and
// store.Open returns an error, so ardvark cannot start. This test asserts that
// store.Open instead succeeds by discarding the stale frontier and rebuilding
// it at the 0.4.1 width.
func TestOpen_MigratesLegacyWideDedupKey(t *testing.T) {
	for _, b := range matrixBackends(t) {
		t.Run(b.driver, func(t *testing.T) {
			raw := openRawMatrix(t, b)

			// Start from a clean, genuinely 0.4.0-shaped table.
			if err := raw.Migrator().DropTable(&legacyFrontierItem{}); err != nil {
				t.Fatalf("dropping any pre-existing frontier_items: %v", err)
			}
			if err := raw.AutoMigrate(&legacyFrontierItem{}); err != nil {
				t.Fatalf("creating legacy varchar(512) frontier_items: %v", err)
			}
			// A realistic 0.4.0 raw key: the "page_fetch:https://..." prefix
			// alone is 22 chars, so any real URL pushes this well past 64.
			longKey := "page_fetch:https://example.com/some/deep/path/to/a/page/that/is/quite/long/indeed.html"
			if len(longKey) <= 64 {
				t.Fatalf("test fixture bug: raw key is only %d chars, must exceed 64", len(longKey))
			}
			if err := raw.Create(&legacyFrontierItem{DedupKey: longKey}).Error; err != nil {
				t.Fatalf("seeding legacy row: %v", err)
			}

			// The upgrade: opening the store runs the reconcile + AutoMigrate.
			st, err := Open(b.driver, b.dsn)
			if err != nil {
				t.Fatalf("store.Open on populated 0.4.0 frontier must succeed, got: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			// The stale, un-narrowable frontier was discarded and rebuilt.
			var count int64
			if err := st.DB.Model(&FrontierItem{}).Count(&count).Error; err != nil {
				t.Fatalf("counting reconciled frontier: %v", err)
			}
			if count != 0 {
				t.Fatalf("expected reconciled frontier to be empty (rows re-discovered on next crawl), got %d", count)
			}

			// The column is now the 0.4.1 fixed width, so subsequent Opens are
			// no-ops (the reconcile only drops when the width exceeds 64).
			cols, err := st.DB.Migrator().ColumnTypes(&FrontierItem{})
			if err != nil {
				t.Fatalf("inspecting reconciled columns: %v", err)
			}
			var checked bool
			for _, c := range cols {
				if c.Name() != "dedup_key" {
					continue
				}
				checked = true
				if length, ok := c.Length(); ok && length != 64 {
					t.Fatalf("expected reconciled dedup_key varchar(64), got varchar(%d)", length)
				}
			}
			if !checked {
				t.Fatal("dedup_key column missing after reconcile")
			}

			// Idempotence: a second Open on the reconciled schema must not drop
			// or error.
			st2, err := Open(b.driver, b.dsn)
			if err != nil {
				t.Fatalf("second store.Open on reconciled frontier: %v", err)
			}
			_ = st2.Close()
		})
	}
}

// TestMigrateFrontierDedupKey_SqliteNoOp guards the dialect gate: sqlite never
// enforces varchar length, so the reconcile must never drop its frontier even
// when a row is present (dropping would silently discard a single-process
// crawler's whole queue on every Open). This runs without any env DSN.
func TestMigrateFrontierDedupKey_SqliteNoOp(t *testing.T) {
	st, err := Open("sqlite", "file:migratenoop?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.DB.Create(&FrontierItem{Kind: KindHostProbe, Host: "example.com", DedupKey: "abc"}).Error; err != nil {
		t.Fatalf("seeding sqlite frontier: %v", err)
	}

	if err := migrateFrontierDedupKey(st.DB); err != nil {
		t.Fatalf("migrateFrontierDedupKey on sqlite: %v", err)
	}

	var count int64
	if err := st.DB.Model(&FrontierItem{}).Count(&count).Error; err != nil {
		t.Fatalf("counting after reconcile: %v", err)
	}
	if count != 1 {
		t.Fatalf("sqlite reconcile must be a no-op; expected 1 row preserved, got %d", count)
	}
}

func TestOpen_DiscardsRowsFromOlderDedupSemantics(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "dedup-version.db")
	st, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := st.DB.Create(&FrontierItem{
		Kind:     KindArtifactFetch,
		URL:      "https://example.com/shared.json",
		Host:     "example.com",
		Status:   FrontierStatusPending,
		DedupKey: "legacy-url-only-key",
	}).Error; err != nil {
		t.Fatalf("seeding legacy frontier row: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing first store: %v", err)
	}

	reopened, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	var count int64
	if err := reopened.DB.Model(&FrontierItem{}).Count(&count).Error; err != nil {
		t.Fatalf("counting frontier rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("frontier rows after dedup-semantics migration = %d, want 0", count)
	}
}

// -- concurrent first open --------------------------------------------------

// dropAllTables returns the database to a pre-migration state so the next
// Open has to create the whole schema.
func dropAllTables(t *testing.T, b matrixBackend) {
	t.Helper()
	db := openRawMatrix(t, b)
	if err := db.Migrator().DropTable(allModels...); err != nil {
		t.Fatalf("dropping tables on %s: %v", b.driver, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("unwrapping %s handle: %v", b.driver, err)
	}
	_ = sqlDB.Close()
}

// A worker fleet starts its processes at once, so several of them call Open
// against a database whose schema does not exist yet. AutoMigrate issues
// CREATE TABLE/INDEX per model, and on a server engine the loser of a
// concurrent create gets a catalog unique-violation
// (pg_class_relname_nsp_index / pg_type_typname_nsp_index on postgres,
// "table already exists" on mysql) rather than a benign no-op — so the worker
// exits before it has even opened its event log.
func TestOpen_ConcurrentFirstOpenIsSafe(t *testing.T) {
	for _, b := range matrixBackends(t) {
		t.Run(b.driver, func(t *testing.T) {
			dropAllTables(t, b)

			const openers = 10
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan error, openers)

			for i := 0; i < openers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start // release them together to maximize overlap
					st, err := Open(b.driver, b.dsn)
					if err == nil {
						_ = st.Close()
					}
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)

			var failed int
			for err := range errs {
				if err != nil {
					failed++
					t.Errorf("concurrent Open failed: %v", err)
				}
			}
			if failed > 0 {
				t.Fatalf("%d of %d concurrent opens failed against %s", failed, openers, b.driver)
			}
		})
	}
}
