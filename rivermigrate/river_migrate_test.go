package rivermigrate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/riverqueue/river/internal/riverinternaltest"
	"github.com/riverqueue/river/internal/util/dbutil"
	"github.com/riverqueue/river/internal/util/sliceutil"
	"github.com/riverqueue/river/riverdriver"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

//nolint:gochecknoglobals
var (
	// We base our test migrations on the actual line of migrations, so get
	// their maximum version number which we'll use to define test version
	// numbers so that the tests don't break anytime we add a new one.
	riverMigrationsMaxVersion = riverMigrations[len(riverMigrations)-1].Version

	testVersions = []*migrationBundle{
		{
			Version: riverMigrationsMaxVersion + 1,
			Up:      "CREATE TABLE test_table(id bigserial PRIMARY KEY);",
			Down:    "DROP TABLE test_table;",
		},
		{
			Version: riverMigrationsMaxVersion + 2,
			Up:      "ALTER TABLE test_table ADD COLUMN name varchar(200); CREATE INDEX idx_test_table_name ON test_table(name);",
			Down:    "DROP INDEX idx_test_table_name; ALTER TABLE test_table DROP COLUMN name;",
		},
	}

	riverMigrationsWithtestVersionsMap        = validateAndInit(append(riverMigrations, testVersions...))
	riverMigrationsWithTestVersionsMaxVersion = riverMigrationsMaxVersion + len(testVersions)
)

func TestMigrator(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	type testBundle struct {
		dbPool *pgxpool.Pool
		driver *riverpgxv5.Driver
		logger *slog.Logger
		tx     pgx.Tx
	}

	setup := func(t *testing.T) (*Migrator[pgx.Tx], *testBundle) {
		t.Helper()

		// The test suite largely works fine with test transactions, but due to
		// the invasive nature of changing schemas, it's quite easy to have test
		// transactions deadlock with each other as they run in parallel. Here
		// we use test DBs instead of test transactions, but this could be
		// changed to test transactions as long as test cases were made to run
		// non-parallel.
		dbPool := riverinternaltest.TestDB(ctx, t)

		// Despite being in an isolated database, we still start a transaction
		// because we don't want schema changes we make to persist.
		tx, err := dbPool.Begin(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback(ctx) })

		bundle := &testBundle{
			dbPool: dbPool,
			driver: riverpgxv5.New(dbPool),
			logger: riverinternaltest.Logger(t),
			tx:     tx,
		}

		migrator := New(bundle.driver, &Config{Logger: bundle.logger})
		migrator.migrations = riverMigrationsWithtestVersionsMap

		return migrator, bundle
	}

	// Gets a migrator using the driver for `database/sql`.
	setupDatabaseSQLMigrator := func(t *testing.T, bundle *testBundle) (*Migrator[*sql.Tx], *sql.Tx) {
		t.Helper()

		stdPool := stdlib.OpenDBFromPool(bundle.dbPool)
		t.Cleanup(func() { require.NoError(t, stdPool.Close()) })

		tx, err := stdPool.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, tx.Rollback()) })

		driver := riverdatabasesql.New(stdPool)
		migrator := New(driver, &Config{Logger: bundle.logger})
		migrator.migrations = riverMigrationsWithtestVersionsMap

		return migrator, tx
	}

	t.Run("MigrateDownDefault", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		// Run an initial time. Defaults to only running one step when moving in
		// the down direction.
		{
			res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionDown, &MigrateOpts{})
			require.NoError(t, err)
			require.Equal(t, DirectionDown, res.Direction)
			require.Equal(t, []int{3}, sliceutil.Map(res.Versions, migrateVersionToInt))

			err = dbExecError(ctx, bundle.driver.UnwrapExecutor(bundle.tx), "SELECT * FROM river_job")
			require.NoError(t, err)
		}

		// Run once more to go down one more step
		{
			res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionDown, &MigrateOpts{})
			require.NoError(t, err)
			require.Equal(t, DirectionDown, res.Direction)
			require.Equal(t, []int{2}, sliceutil.Map(res.Versions, migrateVersionToInt))

			err = dbExecError(ctx, bundle.driver.UnwrapExecutor(bundle.tx), "SELECT * FROM river_job")
			require.Error(t, err)
		}
	})

	t.Run("MigrateDownAfterUp", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{})
		require.NoError(t, err)

		res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionDown, &MigrateOpts{})
		require.NoError(t, err)
		require.Equal(t, []int{riverMigrationsWithTestVersionsMaxVersion}, sliceutil.Map(res.Versions, migrateVersionToInt))
	})

	t.Run("MigrateDownWithMaxSteps", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{})
		require.NoError(t, err)

		res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionDown, &MigrateOpts{MaxSteps: 2})
		require.NoError(t, err)
		require.Equal(t, []int{riverMigrationsWithTestVersionsMaxVersion, riverMigrationsWithTestVersionsMaxVersion - 1},
			sliceutil.Map(res.Versions, migrateVersionToInt))

		migrations, err := bundle.driver.UnwrapExecutor(bundle.tx).MigrationGetAll(ctx)
		require.NoError(t, err)
		require.Equal(t, seqOneTo(riverMigrationsWithTestVersionsMaxVersion-2),
			sliceutil.Map(migrations, migrationToInt))

		err = dbExecError(ctx, bundle.driver.UnwrapExecutor(bundle.tx), "SELECT name FROM test_table")
		require.Error(t, err)
	})

	t.Run("MigrateDownWithPool", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		// We don't actually migrate anything (max steps = -1) because doing so
		// would mess with the test database, but this still runs most code to
		// check that the function generally works.
		res, err := migrator.Migrate(ctx, DirectionDown, &MigrateOpts{MaxSteps: -1})
		require.NoError(t, err)
		require.Equal(t, []int{}, sliceutil.Map(res.Versions, migrateVersionToInt))

		migrations, err := bundle.driver.UnwrapExecutor(bundle.tx).MigrationGetAll(ctx)
		require.NoError(t, err)
		require.Equal(t, seqOneTo(3),
			sliceutil.Map(migrations, migrationToInt))
	})

	t.Run("MigrateDownWithDatabaseSQLDriver", func(t *testing.T) {
		t.Parallel()

		_, bundle := setup(t)
		migrator, tx := setupDatabaseSQLMigrator(t, bundle)

		res, err := migrator.MigrateTx(ctx, tx, DirectionDown, &MigrateOpts{MaxSteps: 1})
		require.NoError(t, err)
		require.Equal(t, []int{3}, sliceutil.Map(res.Versions, migrateVersionToInt))

		migrations, err := migrator.driver.UnwrapExecutor(tx).MigrationGetAll(ctx)
		require.NoError(t, err)
		require.Equal(t, seqOneTo(2),
			sliceutil.Map(migrations, migrationToInt))
	})

	t.Run("MigrateDownWithTargetVersion", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{})
		require.NoError(t, err)

		res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionDown, &MigrateOpts{TargetVersion: 3})
		require.NoError(t, err)
		require.Equal(t, []int{5, 4},
			sliceutil.Map(res.Versions, migrateVersionToInt))

		migrations, err := bundle.driver.UnwrapExecutor(bundle.tx).MigrationGetAll(ctx)
		require.NoError(t, err)
		require.Equal(t, seqOneTo(3),
			sliceutil.Map(migrations, migrationToInt))

		err = dbExecError(ctx, bundle.driver.UnwrapExecutor(bundle.tx), "SELECT name FROM test_table")
		require.Error(t, err)
	})

	t.Run("MigrateDownWithTargetVersionMinusOne", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{})
		require.NoError(t, err)

		res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionDown, &MigrateOpts{TargetVersion: -1})
		require.NoError(t, err)
		require.Equal(t, seqToOne(5),
			sliceutil.Map(res.Versions, migrateVersionToInt))

		err = dbExecError(ctx, bundle.driver.UnwrapExecutor(bundle.tx), "SELECT name FROM river_migrate")
		require.Error(t, err)
	})

	t.Run("MigrateDownWithTargetVersionInvalid", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		// migration doesn't exist
		{
			_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionDown, &MigrateOpts{TargetVersion: 77})
			require.EqualError(t, err, "version 77 is not a valid River migration version")
		}

		// migration exists but not one that's applied
		{
			_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionDown, &MigrateOpts{TargetVersion: 4})
			require.EqualError(t, err, "version 4 is not in target list of valid migrations to apply")
		}
	})

	t.Run("MigrateNilOpts", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, nil)
		require.NoError(t, err)
		require.Equal(t, []int{4, 5}, sliceutil.Map(res.Versions, migrateVersionToInt))
	})

	t.Run("MigrateUpDefault", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		// Run an initial time
		{
			res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{})
			require.NoError(t, err)
			require.Equal(t, DirectionUp, res.Direction)
			require.Equal(t, []int{riverMigrationsWithTestVersionsMaxVersion - 1, riverMigrationsWithTestVersionsMaxVersion},
				sliceutil.Map(res.Versions, migrateVersionToInt))

			migrations, err := bundle.driver.UnwrapExecutor(bundle.tx).MigrationGetAll(ctx)
			require.NoError(t, err)
			require.Equal(t, seqOneTo(riverMigrationsWithTestVersionsMaxVersion),
				sliceutil.Map(migrations, migrationToInt))

			_, err = bundle.tx.Exec(ctx, "SELECT * FROM test_table")
			require.NoError(t, err)
		}

		// Run once more to verify idempotency
		{
			res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{})
			require.NoError(t, err)
			require.Equal(t, DirectionUp, res.Direction)
			require.Equal(t, []int{}, sliceutil.Map(res.Versions, migrateVersionToInt))

			migrations, err := bundle.driver.UnwrapExecutor(bundle.tx).MigrationGetAll(ctx)
			require.NoError(t, err)
			require.Equal(t, seqOneTo(riverMigrationsWithTestVersionsMaxVersion),
				sliceutil.Map(migrations, migrationToInt))

			_, err = bundle.tx.Exec(ctx, "SELECT * FROM test_table")
			require.NoError(t, err)
		}
	})

	t.Run("MigrateUpWithMaxSteps", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{MaxSteps: 1})
		require.NoError(t, err)
		require.Equal(t, []int{riverMigrationsWithTestVersionsMaxVersion - 1},
			sliceutil.Map(res.Versions, migrateVersionToInt))

		migrations, err := bundle.driver.UnwrapExecutor(bundle.tx).MigrationGetAll(ctx)
		require.NoError(t, err)
		require.Equal(t, seqOneTo(riverMigrationsWithTestVersionsMaxVersion-1),
			sliceutil.Map(migrations, migrationToInt))

		// Column `name` is only added in the second test version.
		err = dbExecError(ctx, bundle.driver.UnwrapExecutor(bundle.tx), "SELECT name FROM test_table")
		require.Error(t, err)

		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, pgerrcode.UndefinedColumn, pgErr.Code)
	})

	t.Run("MigrateUpWithPool", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		// We don't actually migrate anything (max steps = -1) because doing so
		// would mess with the test database, but this still runs most code to
		// check that the function generally works.
		res, err := migrator.Migrate(ctx, DirectionUp, &MigrateOpts{MaxSteps: -1})
		require.NoError(t, err)
		require.Equal(t, []int{}, sliceutil.Map(res.Versions, migrateVersionToInt))

		migrations, err := bundle.driver.UnwrapExecutor(bundle.tx).MigrationGetAll(ctx)
		require.NoError(t, err)
		require.Equal(t, seqOneTo(3),
			sliceutil.Map(migrations, migrationToInt))
	})

	t.Run("MigrateUpWithDatabaseSQLDriver", func(t *testing.T) {
		t.Parallel()

		_, bundle := setup(t)
		migrator, tx := setupDatabaseSQLMigrator(t, bundle)

		res, err := migrator.MigrateTx(ctx, tx, DirectionUp, &MigrateOpts{MaxSteps: 1})
		require.NoError(t, err)
		require.Equal(t, []int{riverMigrationsMaxVersion + 1}, sliceutil.Map(res.Versions, migrateVersionToInt))

		migrations, err := migrator.driver.UnwrapExecutor(tx).MigrationGetAll(ctx)
		require.NoError(t, err)
		require.Equal(t, seqOneTo(riverMigrationsMaxVersion+1),
			sliceutil.Map(migrations, migrationToInt))
	})

	t.Run("MigrateUpWithTargetVersion", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		res, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{TargetVersion: 5})
		require.NoError(t, err)
		require.Equal(t, []int{4, 5},
			sliceutil.Map(res.Versions, migrateVersionToInt))

		migrations, err := bundle.driver.UnwrapExecutor(bundle.tx).MigrationGetAll(ctx)
		require.NoError(t, err)
		require.Equal(t, seqOneTo(5), sliceutil.Map(migrations, migrationToInt))
	})

	t.Run("MigrateUpWithTargetVersionInvalid", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		// migration doesn't exist
		{
			_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{TargetVersion: 77})
			require.EqualError(t, err, "version 77 is not a valid River migration version")
		}

		// migration exists but already applied
		{
			_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{TargetVersion: 3})
			require.EqualError(t, err, "version 3 is not in target list of valid migrations to apply")
		}
	})

	t.Run("ValidateSuccess", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		// Migrate all the way up.
		_, err := migrator.MigrateTx(ctx, bundle.tx, DirectionUp, &MigrateOpts{})
		require.NoError(t, err)

		res, err := migrator.ValidateTx(ctx, bundle.tx)
		require.NoError(t, err)
		require.Equal(t, &ValidateResult{OK: true}, res)
	})

	t.Run("ValidateUnappliedMigrations", func(t *testing.T) {
		t.Parallel()

		migrator, bundle := setup(t)

		res, err := migrator.ValidateTx(ctx, bundle.tx)
		require.NoError(t, err)
		require.Equal(t, &ValidateResult{
			Messages: []string{fmt.Sprintf("Unapplied migrations: [%d %d]", riverMigrationsMaxVersion+1, riverMigrationsMaxVersion+2)},
		}, res)
	})
}

// A command returning an error aborts the transaction. This is a shortcut to
// execute a command in a subtransaction so that we can verify an error, but
// continue to use the original transaction.
func dbExecError(ctx context.Context, exec riverdriver.Executor, sql string) error {
	return dbutil.WithTx(ctx, exec, func(ctx context.Context, exec riverdriver.ExecutorTx) error {
		_, err := exec.Exec(ctx, sql)
		return err
	})
}

func migrationToInt(r *riverdriver.Migration) int { return r.Version }

func seqOneTo(max int) []int {
	seq := make([]int, max)

	for i := 0; i < max; i++ {
		seq[i] = i + 1
	}

	return seq
}

func seqToOne(max int) []int {
	seq := seqOneTo(max)
	slices.Reverse(seq)
	return seq
}
