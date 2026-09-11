package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/glebarez/sqlite"
	// Registers the "pgx" database/sql driver used by openPostgresSessionStore.
	// gorm.io/driver/postgres imports the same shim, but a driver this file opens
	// by name is a dependency this file must declare.
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"gorm.io/driver/postgres"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// postgresDriverName is the database/sql name that pgx/v5's stdlib shim
// registers. pgx exports no constant for it, so it is spelled once here.
const postgresDriverName = "pgx"

// sessionStore makes database ownership explicit around ADK v2.3.0's session
// service. The upstream concrete service owns a GORM handle but exposes no
// Close, so the repository supplies and retains the database/sql pool itself.
//
// Field order follows what go vet's fieldalignment check wants — every
// pointer-bearing field ahead of the pointer-free sync.Once — rather than how
// the fields group by meaning.
type sessionStore struct {
	session.Service

	underlying session.Service
	closeErr   error
	pool       *sql.DB
	// backend records which engine this store opened, so error messages can name
	// the right artifact — a file path or a server — without a second lookup.
	backend   config.SessionBackend
	closeOnce sync.Once
}

// openSessionStore opens the configured persistent session store without
// migrating it.
//
// The two backends answer the same question — where do sessions live — with
// different concurrency contracts, so they are separate constructors rather than
// one function with branches sprinkled through it.
func openSessionStore(cfg config.Config, recovered recoveredState) (*sessionStore, error) {
	switch cfg.SessionBackend {
	// The empty value is the zero value of a Config built in code rather than
	// parsed from the environment, and the shipped envDefault is sqlite. Treating
	// the two the same keeps "unset means the account-free default" true in both
	// directions, while a non-empty value the switch does not know still refuses.
	case config.SessionBackendSQLite, "":
		return openSQLiteSessionStore(cfg, recovered)
	case config.SessionBackendPostgres:
		return openPostgresSessionStore(cfg)
	default:
		// Load rejects an unknown backend, so reaching this is a Config built in
		// code rather than parsed from the environment. Refusing beats defaulting:
		// a typo that silently opened SQLite would look like a working deployment
		// right up to the second replica.
		return nil, fmt.Errorf(
			"%s=%q has no session store implementation; run `%s` for the accepted values",
			config.EnvSessionBackend, cfg.SessionBackend, configCheckCommand,
		)
	}
}

// openSQLiteSessionStore opens the single-writer file store in AGENT_STATE_DIR.
//
// A recoveredState token is required because GORM connects eagerly while it
// builds the service, and that connection must never resolve an unrecovered
// runtime.db pathname.
func openSQLiteSessionStore(cfg config.Config, recovered recoveredState) (*sessionStore, error) {
	if err := recovered.require(cfg.StateDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating the state directory %s: %w", cfg.StateDir, err)
	}

	pool, err := sql.Open(sqlite.DriverName, a2aserver.SessionDataSourceName(cfg.StateDir))
	if err != nil {
		return nil, fmt.Errorf("opening the session database pool in %s: %w", cfg.StateDir, err)
	}
	// ADK's transactions read app/user state before writing session state. One
	// connection plus BEGIN IMMEDIATE is the bounded serialization contract
	// documented by SessionDataSourceName.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)

	underlying, err := database.NewSessionService(&sqlite.Dialector{Conn: pool})
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("opening the session store in %s: %w", cfg.StateDir, err),
			pool.Close(),
		)
	}
	return newSessionStore(underlying, pool, config.SessionBackendSQLite), nil
}

// openPostgresSessionStore opens the shared session database every replica
// writes to.
//
// Nothing here touches AGENT_STATE_DIR, which is the point: the sessions are no
// longer on this replica's disk, so the serialization that SQLite bought with a
// one-connection pool is bought instead by the server's own row locks, and two
// processes appending events to one conversation is an ordinary transaction
// rather than a corrupted file.
//
// The pool is bounded because the ceiling that matters here is shared. Replicas
// multiplied by AGENT_SESSION_MAX_CONNS must stay under the server's
// max_connections; an unbounded pool turns one traffic spike into "sorry, too
// many clients already" for every replica at once, including the healthy ones.
func openPostgresSessionStore(cfg config.Config) (*sessionStore, error) {
	pool, err := newPostgresPool(cfg)
	if err != nil {
		return nil, err
	}

	// GORM pings while it builds the service, so an unreachable or unauthorized
	// server fails here rather than on the first turn. pgx renders that failure
	// as user and database plus the address — never the password — so it is safe
	// to carry the cause.
	underlying, err := database.NewSessionService(postgres.New(postgres.Config{Conn: pool}))
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("opening the session store named by %s: %w", config.EnvSessionDSN, err),
			pool.Close(),
		)
	}
	return newSessionStore(underlying, pool, config.SessionBackendPostgres), nil
}

// newPostgresPool opens the connection pool the shared session store runs on.
//
// database/sql dials nothing here, so this is a pure parse-and-bound step: the
// first connection is made by the ping GORM issues next. Keeping it separate is
// what lets the pool's ceiling be asserted without a server.
func newPostgresPool(cfg config.Config) (*sql.DB, error) {
	pool, err := sql.Open(postgresDriverName, cfg.SessionDSN.Reveal())
	if err != nil {
		// Only an unregistered driver name reaches this: pgx defers parsing the
		// connection string to the first connect, so a malformed DSN surfaces from
		// the ping below instead. The blank import above is what keeps the name
		// registered, and this branch is what would say so if it ever stopped.
		return nil, fmt.Errorf("the %q database driver is not registered: %w", postgresDriverName, err)
	}
	pool.SetMaxOpenConns(cfg.SessionMaxConns)
	pool.SetMaxIdleConns(cfg.SessionMaxConns)
	return pool, nil
}

// newSessionStore wraps an opened service with the pool and backend it owns.
func newSessionStore(
	underlying session.Service, pool *sql.DB, backend config.SessionBackend,
) *sessionStore {
	return &sessionStore{
		Service:    underlying,
		underlying: underlying,
		pool:       pool,
		backend:    backend,
	}
}

// migrate creates ADK's schema through the concrete service it returned. The
// upstream AutoMigrate intentionally rejects interface wrappers, which is why
// the wrapper retains both the delegated interface and the concrete value.
func (s *sessionStore) migrate(stateDir string) error {
	if err := database.AutoMigrate(s.underlying); err != nil {
		return fmt.Errorf("migrating the session store %s: %w", s.describe(stateDir), err)
	}
	return nil
}

// describe names the store the way an operator would look for it, and never
// renders the DSN: the Postgres form is a credential.
func (s *sessionStore) describe(stateDir string) string {
	if s.backend == config.SessionBackendPostgres {
		return "on the PostgreSQL server named by " + config.EnvSessionDSN
	}
	return filepath.Join(stateDir, a2aserver.SessionDatabaseName)
}

// readinessProbe returns the session-store check the A2A readiness route must
// run, or nil when that package can answer for itself.
//
// The file backend keeps its sessions inside AGENT_STATE_DIR, which a2aserver
// opens read-only on its own; there is nothing this process could add, and
// supplying a probe there is refused rather than merely redundant. A shared
// server is reachable only through the pool this store owns, so the check that
// asks whether the session schema is live has to be handed over from here.
func (s *sessionStore) readinessProbe() a2aserver.Step {
	if s.backend != config.SessionBackendPostgres {
		return nil
	}
	return func(ctx context.Context) error {
		return a2aserver.ProbePostgresSessionStore(ctx, s.pool)
	}
}

// Close releases the exact pool supplied to GORM. It is idempotent so either a
// normal shutdown or an error-unwind can safely converge on the same owner.
func (s *sessionStore) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.pool.Close() })
	return s.closeErr
}

// newSessionService opens and migrates the launcher path's session store.
func newSessionService(cfg config.Config, recovered recoveredState) (*sessionStore, error) {
	sessions, err := openSessionStore(cfg, recovered)
	if err != nil {
		return nil, err
	}
	if err := sessions.migrate(cfg.StateDir); err != nil {
		return nil, errors.Join(err, sessions.Close())
	}
	return sessions, nil
}
