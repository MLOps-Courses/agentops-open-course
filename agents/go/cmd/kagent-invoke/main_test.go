package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	semconv "go.opentelemetry.io/otel/semconv/v1.36.0"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/kagentinterop"
)

type fakeInvoker struct {
	contextID string
	task      string
}

func (f *fakeInvoker) Invoke(_ context.Context, task, contextID string) (kagentinterop.Result, error) {
	f.task = task
	f.contextID = contextID
	return kagentinterop.Result{
		Agent: kagentinterop.IncidentReaderRef, Text: "bounded evidence", ContextID: "context-next",
	}, nil
}

func (*fakeInvoker) Close() error { return nil }

func TestRunInvokesTheFixedSpecialistAndPrintsStructuredResult(t *testing.T) {
	invoker := &fakeInvoker{}
	task := "Investigate " + domain.Reference().Incidents.InventoryDown + "."
	var gotConfig kagentinterop.Config
	dial := func(_ context.Context, cfg kagentinterop.Config) (client, error) {
		gotConfig = cfg
		return invoker, nil
	}
	var stdout bytes.Buffer

	err := run(t.Context(), []string{
		"--endpoint", "http://127.0.0.1:8083/mcp",
		"--timeout", "45s",
		"--task", task,
		"--context-id", "context-first",
	}, &stdout, &bytes.Buffer{}, dial)
	if err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if gotConfig.Endpoint != "http://127.0.0.1:8083/mcp" || gotConfig.Timeout.String() != "45s" {
		t.Errorf("dial config = %+v, want the parsed endpoint and timeout", gotConfig)
	}
	if invoker.task != task || invoker.contextID != "context-first" {
		t.Errorf("invocation = task %q context %q, want caller values", invoker.task, invoker.contextID)
	}
	var result kagentinterop.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not result JSON: %v", err)
	}
	if result.Agent != kagentinterop.IncidentReaderRef || result.ContextID != "context-next" {
		t.Errorf("result = %+v, want fixed specialist and resumable context", result)
	}
}

func TestRunRejectsMissingTaskBeforeDialing(t *testing.T) {
	dialed := false
	dial := func(context.Context, kagentinterop.Config) (client, error) {
		dialed = true
		return &fakeInvoker{}, nil
	}
	if err := run(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}, dial); err == nil {
		t.Fatal("run() error = nil, want missing --task to fail")
	}
	if dialed {
		t.Fatal("invalid arguments reached the remote dialer")
	}
}

func TestClientTelemetryUsesItsOwnServiceIdentity(t *testing.T) {
	resource, err := clientTelemetryResource(buildinfo.Info{
		Mode:           buildinfo.Development,
		Version:        buildinfo.DevelopmentVersion,
		SourceIdentity: buildinfo.DevelopmentIdentity,
		Dirty:          true,
	})
	if err != nil {
		t.Fatalf("clientTelemetryResource() error = %v, want nil", err)
	}
	serviceName, found := resource.Set().Value(semconv.ServiceNameKey)
	if !found {
		t.Fatal("client telemetry resource has no service.name")
	}
	if got := serviceName.AsString(); got != clientServiceName {
		t.Errorf("service.name = %q, want %q", got, clientServiceName)
	}
}
