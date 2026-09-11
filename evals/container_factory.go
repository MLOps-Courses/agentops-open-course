package evals

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

type ContainerClientFactoryConfig struct {
	Output      io.Writer
	Environment map[string]string
	Engine      string
	Image       string
	Transport   string
	Entrypoint  string
	AppName     string
	Source      SourceEvidence
	Timeout     time.Duration
	Streaming   bool
}

func NewContainerClientFactory(ctx context.Context, config ContainerClientFactoryConfig) (ClientFactory, error) {
	return newContainerClientFactory(ctx, config, executeRuntimeCommand)
}

func newContainerClientFactory(
	ctx context.Context, config ContainerClientFactoryConfig, run runtimeCommand,
) (ClientFactory, error) {
	if config.Engine == "" {
		config.Engine = defaultContainerEngine
	}
	if config.Image == "" {
		return nil, errors.New("container client factory needs the agent image")
	}
	if config.Transport != "rest" && config.Transport != "a2a" {
		return nil, fmt.Errorf("unsupported container client transport %q", config.Transport)
	}
	if config.Transport == "a2a" && config.Entrypoint != "agent" {
		return nil, errors.New("the deployed A2A contract serves only the agent entrypoint")
	}
	verifiedImage, err := resolveContainerRuntimeIdentity(ctx, config.Engine, config.Image, config.Source, run)
	if err != nil {
		return nil, err
	}
	config.Image = verifiedImage
	return func(ctx context.Context, evalCase EvalCase, _ int) (AgentClient, func() error, error) {
		port, err := FreePort()
		if err != nil {
			return nil, nil, err
		}
		container, err := StartAgentContainer(ctx, AgentContainerConfig{
			Engine: config.Engine, Image: config.Image,
			Transport: config.Transport, Entrypoint: config.Entrypoint, Port: port,
			Environment: config.Environment, Output: config.Output,
		})
		if err != nil {
			return nil, nil, err
		}
		cleanup := container.Close
		if config.Transport == "a2a" {
			a2aClient, clientErr := NewA2AClient(A2AClientConfig{BaseURL: container.BaseURL(), Timeout: config.Timeout})
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
			BaseURL: container.BaseURL(), AppName: appName, UserID: evalCase.SessionInput.UserID,
			Streaming: config.Streaming, Timeout: config.Timeout,
		})
		if err != nil {
			_ = cleanup()
			return nil, nil, err
		}
		return client, cleanup, nil
	}, nil
}
