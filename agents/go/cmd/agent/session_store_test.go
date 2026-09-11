package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// postgresTestDSN carries a password on purpose. Every assertion below reads it
// back out of an error message, because "the DSN never reaches a log" is a
// promise this store makes and a container log is forever.
const postgresTestDSN = "postgres://agentops:hunter2@127.0.0.1:1/sessions?sslmode=disable"

// TestSQLiteSessionStoreKeepsOneWriter pins the serialization contract the file
// backend rests on. ADK reads app and user state before it writes session
// state, so a second connection is a second transaction that SQLite refuses
// rather than queues.
func TestSQLiteSessionStoreKeepsOneWriter(t *testing.T) {
	t.Parallel()

	cfg := config.Config{StateDir: filepath.Join(t.TempDir(), "state")}
	store, err := openSessionStore(cfg, recoveredTestState(t, cfg))
	if err != nil {
		t.Fatalf("openSessionStore() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	if got := store.pool.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want the single-writer bound of 1", got)
	}
	if store.backend != config.SessionBackendSQLite {
		t.Errorf("backend = %q, want %q for an unset setting", store.backend, config.SessionBackendSQLite)
	}
}

// TestSessionStoreRefusesAnUnknownBackend covers the Config-built-in-code path
// that Load cannot police. Defaulting to SQLite on a typo would look like a
// working deployment right up to the second replica.
func TestSessionStoreRefusesAnUnknownBackend(t *testing.T) {
	t.Parallel()

	cfg := config.Config{StateDir: t.TempDir(), SessionBackend: "mysql"}
	store, err := openSessionStore(cfg, recoveredTestState(t, cfg))
	if err == nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
		t.Fatal("openSessionStore() error = nil for an unknown backend")
	}
	assertMessageContains(t, err.Error(), config.EnvSessionBackend, `"mysql"`, configCheckCommand)
}

// TestPostgresSessionStoreBoundsItsPoolPerReplica proves the setting reaches
// database/sql. The ceiling that matters is shared: replicas multiplied by this
// number must stay under the server's max_connections.
func TestPostgresSessionStoreBoundsItsPoolPerReplica(t *testing.T) {
	t.Parallel()

	pool, err := newPostgresPool(config.Config{
		SessionBackend:  config.SessionBackendPostgres,
		SessionDSN:      config.Secret(postgresTestDSN),
		SessionMaxConns: 4,
	})
	if err != nil {
		t.Fatalf("newPostgresPool() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	stats := pool.Stats()
	if stats.MaxOpenConnections != 4 {
		t.Errorf("MaxOpenConnections = %d, want the configured 4", stats.MaxOpenConnections)
	}
	// Opening the pool must dial nothing: the first connection belongs to the
	// ping GORM issues, which is what makes an unreachable server a startup
	// failure rather than a first-turn failure.
	if stats.OpenConnections != 0 {
		t.Errorf("OpenConnections = %d, want 0 before the store is built", stats.OpenConnections)
	}
}

// TestPostgresSessionStoreRejectsADriverUnparseableDSN covers the gap between
// the two validators: config checks URL shape, and pgx checks libpq semantics
// such as the closed sslmode set. A value that clears the first and fails the
// second must still fail at startup, and still without the password.
func TestPostgresSessionStoreRejectsADriverUnparseableDSN(t *testing.T) {
	t.Parallel()

	dsn := "postgres://agentops:hunter2@127.0.0.1:5432/sessions?sslmode=sometimes"
	if reason := postgresDSNShapeProblem(t, dsn); reason != "" {
		t.Fatalf("the fixture DSN is rejected by config validation (%s); pick one that reaches the driver", reason)
	}
	store, err := openSessionStore(config.Config{
		SessionBackend:  config.SessionBackendPostgres,
		SessionDSN:      config.Secret(dsn),
		SessionMaxConns: 10,
	}, recoveredState{})
	if err == nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
		t.Fatal("openSessionStore() error = nil for a DSN the driver cannot parse")
	}
	assertMessageContains(t, err.Error(), config.EnvSessionDSN, "sslmode")
	assertMessageOmitsTheCredential(t, err.Error(), dsn)
}

// TestPostgresSessionStoreReportsAnUnreachableServer proves a deployment
// pointed at a dead address fails at startup, with a message an operator can
// act on and without the password in it.
func TestPostgresSessionStoreReportsAnUnreachableServer(t *testing.T) {
	t.Parallel()

	// Port 1 is reserved and unbound, so this is a refused connection rather
	// than a timeout, and the test stays fast and offline.
	store, err := openSessionStore(config.Config{
		SessionBackend:  config.SessionBackendPostgres,
		SessionDSN:      config.Secret(postgresTestDSN),
		SessionMaxConns: 10,
	}, recoveredState{})
	if err == nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
		t.Fatal("openSessionStore() error = nil against an unbound port")
	}
	assertMessageContains(t, err.Error(), config.EnvSessionDSN)
	assertMessageOmitsTheCredential(t, err.Error(), postgresTestDSN)
}

// TestPostgresSessionStoreNeedsNoRecoveredStateDirectory records the behavioral
// difference the whole backend exists for: with sessions on a shared server,
// this replica's disk is no longer part of the session contract, so the store
// opens without the state-restore token the file backend requires.
func TestPostgresSessionStoreNeedsNoRecoveredStateDirectory(t *testing.T) {
	t.Parallel()

	// The same zero token against the file backend is refused outright.
	if _, err := openSessionStore(config.Config{StateDir: t.TempDir()}, recoveredState{}); err == nil {
		t.Fatal("openSQLiteSessionStore() error = nil without a recovered state directory")
	}
	pool, err := newPostgresPool(config.Config{
		SessionBackend:  config.SessionBackendPostgres,
		SessionDSN:      config.Secret(postgresTestDSN),
		SessionMaxConns: 10,
	})
	if err != nil {
		t.Fatalf("newPostgresPool() error = %v, want nil without any state directory", err)
	}
	if err := pool.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

// TestTheSessionStoreProbesItselfOnlyWhenItIsShared pins the readiness pairing
// from this side of the seam.
//
// a2aserver opens the file store's runtime.db read-only and refuses a probe it
// did not need; the shared store is reachable only through the pool this
// process owns, so the check has to be handed over. A store that answered for
// the wrong one would be a readiness check reporting on a database no replica
// writes to — which is how a healthy Postgres deployment stays unready forever.
func TestTheSessionStoreProbesItselfOnlyWhenItIsShared(t *testing.T) {
	t.Parallel()

	cfg := config.Config{StateDir: filepath.Join(t.TempDir(), "state")}
	file, err := openSessionStore(cfg, recoveredTestState(t, cfg))
	if err != nil {
		t.Fatalf("openSessionStore() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Errorf("Close() error = %v, want nil", closeErr)
		}
	})
	if file.readinessProbe() != nil {
		t.Error("the file backend supplies a readiness probe, which a2aserver refuses")
	}

	pool, err := newPostgresPool(config.Config{
		SessionBackend:  config.SessionBackendPostgres,
		SessionDSN:      config.Secret(postgresTestDSN),
		SessionMaxConns: 10,
	})
	if err != nil {
		t.Fatalf("newPostgresPool() error = %v, want nil", err)
	}
	// The store is assembled directly because openPostgresSessionStore pings, and
	// the point here is what the probe does once a pool exists.
	shared := &sessionStore{pool: pool, backend: config.SessionBackendPostgres}
	t.Cleanup(func() {
		if closeErr := shared.Close(); closeErr != nil {
			t.Errorf("Close() error = %v, want nil", closeErr)
		}
	})
	probe := shared.readinessProbe()
	if probe == nil {
		t.Fatal("the shared backend supplies no readiness probe; readiness would report on nothing")
	}
	// Port 1 is unbound, so this is a refused connection: the probe asks the
	// server rather than assuming, and says so without the password.
	err = probe(t.Context())
	if err == nil {
		t.Fatal("readinessProbe() error = nil against an unbound port")
	}
	assertMessageContains(t, err.Error(), "PostgreSQL session tables")
	assertMessageOmitsTheCredential(t, err.Error(), postgresTestDSN)
}

// TestSessionStoreNamesItselfWithoutTheDSN pins what a migration failure is
// allowed to say. The SQLite store is a path an operator can open; the Postgres
// store is a credential, so it is named by its variable instead.
func TestSessionStoreNamesItselfWithoutTheDSN(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join("agents", "go", ".state")
	file := &sessionStore{backend: config.SessionBackendSQLite}
	if got, want := file.describe(stateDir), filepath.Join(stateDir, a2aserver.SessionDatabaseName); got != want {
		t.Errorf("describe() = %q, want %q", got, want)
	}
	shared := &sessionStore{backend: config.SessionBackendPostgres}
	got := shared.describe(stateDir)
	if !strings.Contains(got, config.EnvSessionDSN) {
		t.Errorf("describe() = %q, want it to name %s", got, config.EnvSessionDSN)
	}
	if strings.Contains(got, stateDir) {
		t.Errorf("describe() = %q, want no local path for a shared store", got)
	}
}

// postgresDSNShapeProblem reports why config validation would reject dsn, so a
// fixture meant to reach the driver cannot silently stop at the config layer.
func postgresDSNShapeProblem(t *testing.T, dsn string) string {
	t.Helper()

	_, err := config.LoadFrom(map[string]string{
		config.EnvSessionBackend: string(config.SessionBackendPostgres),
		config.EnvSessionDSN:     dsn,
	})
	if err == nil {
		return ""
	}
	return err.Error()
}

// assertMessageContains fails with the full message, which is what an operator
// reads on a bad day.
func assertMessageContains(t *testing.T, message string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Errorf("message does not contain %q\nfull message:\n%s", want, message)
		}
	}
}

// assertMessageOmitsTheCredential is the promise that makes it safe to log this
// error at all.
func assertMessageOmitsTheCredential(t *testing.T, message, dsn string) {
	t.Helper()

	for _, secret := range []string{"hunter2", dsn} {
		if strings.Contains(message, secret) {
			t.Errorf("message leaked the session credential %q\nfull message:\n%s", secret, message)
		}
	}
}
