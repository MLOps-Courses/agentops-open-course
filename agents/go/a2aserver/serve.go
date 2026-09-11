package a2aserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// storeProbeTimeout bounds the readiness check over the two state databases.
// It matches the Python track's asyncio.timeout(5) around the same probe.
const storeProbeTimeout = 5 * time.Second

// stateDirPerm is the mode of the runtime state directory, matching what the
// data package creates it with.
const stateDirPerm = 0o750

// Start runs the startup sequence, in the one order that is safe.
//
// # The order is the guarantee
//
//  1. Recover an interrupted snapshot restore. Until this runs, a state
//     directory holding a half-published generation is indistinguishable from a
//     healthy one, so nothing may read it and nothing may publish into it.
//  2. Refuse an incompatible state generation, read-only. Every byte on disk is
//     still exactly as it was found, which is what makes "select a compatible
//     snapshot" an instruction the operator can act on.
//  3. Publish and migrate the agent-owned incident database. The writable A2A
//     process owns first boot; readiness is a read-only observation afterwards.
//  4. Create ADK's session schema and this package's task schema. Both are
//     lazy, and readiness must not report success before they exist.
//  5. Build the HTTP surface, which is the first moment a request could be
//     served at all.
//
// Steps 1 to 3 are the course invariant recorded in AGENTS.md. Moving the
// recovery after the preflight or the preflight after publication is a
// correctness regression, not a refactor.
// --8<-- [start:a2a-lifespan]
func (s *Server) Start(ctx context.Context) error {
	if err := s.recoverState(ctx); err != nil {
		return fmt.Errorf("recovering an interrupted state restore: %w", err)
	}
	// Recovery treats a missing state directory as a clean first boot. Only
	// after that decision may startup create the directory it will publish into.
	if err := os.MkdirAll(s.options.StateDir, stateDirPerm); err != nil {
		return fmt.Errorf("creating the state directory %s: %w", s.options.StateDir, err)
	}
	if err := preflightStateStores(ctx, s.options.StateDir, s.options.SessionBackend); err != nil {
		return fmt.Errorf("checking runtime state before startup: %w", err)
	}
	if err := s.prepareDataset(ctx); err != nil {
		return fmt.Errorf("preparing the runtime dataset: %w", err)
	}
	if err := s.migrateSessions(ctx); err != nil {
		return fmt.Errorf("preparing the session store: %w", err)
	}

	tasks, err := OpenTaskStore(ctx, TaskStoreConfig{
		Path: filepath.Join(s.options.StateDir, TaskDatabaseName),
	})
	if err != nil {
		return fmt.Errorf("preparing the task store: %w", err)
	}
	s.tasks = tasks

	handler, err := s.buildHandler()
	if err != nil {
		return errors.Join(fmt.Errorf("building the A2A surface: %w", err), s.Close())
	}
	s.handler = handler
	return nil
}

// --8<-- [end:a2a-lifespan]

// Serve runs the startup sequence and then serves until ctx is canceled.
//
// Cancelation is a normal shutdown, not a failure: pressing Ctrl-C on a
// foreground server and Kubernetes sending SIGTERM both arrive here as a
// canceled context, and both return nil.
//
// The drain is what makes a rolling update lossless. On shutdown the listener
// stops accepting, in-flight turns are given DrainTimeout to finish, and only
// then does the process exit. The bound reduces avoidable interruption and
// nothing more: a turn that exceeds it is still cut, which is why every state
// change is transactional rather than relying on drain time for correctness.
// Kubernetes' terminationGracePeriodSeconds must exceed this window.
// --8<-- [start:a2a-cancel]
func (s *Server) Serve(ctx context.Context) (err error) {
	if startErr := s.Start(ctx); startErr != nil {
		return startErr
	}
	defer func() { err = errors.Join(err, s.Close()) }()

	server := &http.Server{
		Addr:              s.Address(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	listening := make(chan error, 1)
	go func() { listening <- server.ListenAndServe() }()

	s.logger.InfoContext(ctx, "serving A2A",
		"address", s.Address(), "advertised", s.URL(), "streaming", s.options.Streaming)

	select {
	case listenErr := <-listening:
		if errors.Is(listenErr, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving A2A on %s: %w", s.Address(), listenErr)
	case <-ctx.Done():
		// WithoutCancel, because the drain deadline has to outlive the
		// cancelation that started it — a shutdown context that is already
		// canceled would drain for exactly zero seconds.
		drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.options.DrainTimeout)
		defer cancel()
		s.logger.InfoContext(ctx, "draining in-flight A2A requests",
			"timeout", s.options.DrainTimeout.String())
		if shutdownErr := server.Shutdown(drain); shutdownErr != nil {
			return fmt.Errorf("draining A2A connections within %s: %w", s.options.DrainTimeout, shutdownErr)
		}
		return nil
	}
}

// --8<-- [end:a2a-cancel]
