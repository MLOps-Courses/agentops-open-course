package evals

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

type ProcessClientFactoryConfig struct {
	Output      io.Writer
	Environment map[string]string
	Binary      string
	DataDir     string
	Transport   string
	Entrypoint  string
	AppName     string
	Source      SourceEvidence
	Timeout     time.Duration
	Streaming   bool
}

// EvalIdentityHeader is the header this harness sets to stand in for the gateway
// on the A2A transport.
//
// The agent grants an authenticated principal only from a header a trusted proxy
// set, and refuses a guarded write without one — so on A2A the harness must be
// that proxy or it can never score an approval. The name is the harness's own
// rather than a deployment's, because a real deployment configures the agent to
// trust exactly one header and this must not look like that one.
const EvalIdentityHeader = "x-agentops-eval-subject"

func NewProcessClientFactory(ctx context.Context, config ProcessClientFactoryConfig) (ClientFactory, error) {
	return newProcessClientFactory(ctx, config, executeRuntimeCommand)
}

func newProcessClientFactory(
	ctx context.Context, config ProcessClientFactoryConfig, run runtimeCommand,
) (ClientFactory, error) {
	if config.Binary == "" || config.DataDir == "" {
		return nil, errors.New("process client factory needs the agent binary and data directory")
	}
	if config.Transport != "rest" && config.Transport != "a2a" {
		return nil, fmt.Errorf("unsupported process client transport %q", config.Transport)
	}
	if config.Transport == "a2a" && config.Entrypoint != "agent" {
		return nil, errors.New("the deployed A2A contract serves only the agent entrypoint")
	}
	if err := verifyRuntimeIdentity(ctx, config.Binary, config.Source, run); err != nil {
		return nil, err
	}
	return func(ctx context.Context, evalCase EvalCase, _ int) (AgentClient, func() error, error) {
		stateDir, removeState, err := NewIsolatedState()
		if err != nil {
			return nil, nil, err
		}
		binary, err := snapshotRuntimeBinary(config.Binary, stateDir)
		if err != nil {
			_ = removeState()
			return nil, nil, err
		}
		if identityErr := verifyRuntimeIdentity(ctx, binary, config.Source, run); identityErr != nil {
			_ = removeState()
			return nil, nil, identityErr
		}
		port, err := FreePort()
		if err != nil {
			_ = removeState()
			return nil, nil, err
		}
		process, err := StartAgentProcess(ctx, AgentProcessConfig{
			Binary: binary, Transport: config.Transport, Entrypoint: config.Entrypoint,
			DataDir: config.DataDir, StateDir: stateDir, Port: port,
			Environment: config.Environment, Output: config.Output,
		})
		if err != nil {
			_ = removeState()
			return nil, nil, err
		}
		cleanup := func() error { return errors.Join(process.Close(), removeState()) }
		if config.Transport == "a2a" {
			a2aClient, clientErr := NewA2AClient(A2AClientConfig{
				BaseURL: process.BaseURL(), Timeout: config.Timeout,
				IdentityHeader: EvalIdentityHeader, Identity: evalCase.SessionInput.UserID,
			})
			if clientErr != nil {
				_ = cleanup()
				return nil, nil, clientErr
			}
			return a2aClient, cleanup, nil
		}
		appName := config.AppName
		if appName == "" {
			appName = evalCase.SessionInput.AppName
		}
		client, err := NewRESTClient(RESTClientConfig{
			BaseURL: process.BaseURL(), AppName: appName, UserID: evalCase.SessionInput.UserID,
			Streaming: config.Streaming, Timeout: config.Timeout,
		})
		if err != nil {
			_ = cleanup()
			return nil, nil, err
		}
		return client, cleanup, nil
	}, nil
}
