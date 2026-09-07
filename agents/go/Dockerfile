# syntax=docker/dockerfile:1.11@sha256:10c699f1b6c8bdc8f6b4ce8974855dd8542f1768c26eb240237b8f1c9c6c9976
# AgentOps Agent — container image (Chapter 6.1).
#
# Two stages, no package manager in the result: the Go toolchain compiles one static,
# cgo-free binary, and a distroless base carries only that binary, the bundled dataset,
# and an empty state directory. Both bases are digest-pinned for reproducible builds.
#
# Why a Dockerfile and not `ko`, which is the Go stack's default image builder: this image
# needs a default `CMD` because kagent's `type: BYO` Agent deploys it with no command of its
# own (infra/kagent/agent.yaml). It also ships the sibling agents/data tree; keeping that
# immutable dataset outside the Go package is an intentional seed/state boundary.
#
# The image serves the agent over A2A on :8080 by default — the contract kagent's BYO Agent
# expects (it deploys this image and probes /.well-known/agent-card.json). The MCP server and
# the state CLI are the same binary under different arguments, which is why the manifests
# override `args` and never `command`.
#
# Build context is the repo's agents/ directory, so both go/ and data/ are visible.
# From the repo root:
# mise run build:agent-image

# --- Stage 1: compile one static, cgo-free binary ----------------------------
# --8<-- [start:base-images]
FROM golang:1.26.6-alpine@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df AS build
# --8<-- [end:base-images]

WORKDIR /src

# The locked module graph first, so the dependency layer stays cached until go.mod or
# go.sum changes rather than on every source edit.
COPY go/go.mod go/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY go/ ./

# One metadata tuple feeds both the binary and the OCI configuration. The
# repository source-identity command supplies these values; the Dockerfile
# refuses missing or templated inputs instead of guessing HEAD.
ARG AGENT_BUILD_MODE
ARG AGENT_SOURCE_COMMIT
ARG AGENT_SOURCE_REVISION
ARG AGENT_SOURCE_TREE_DIGEST
ARG AGENT_SOURCE_DIRTY
ARG OCI_CREATED
ARG OCI_VERSION

# --8<-- [start:traceability-guard]
RUN case "${AGENT_BUILD_MODE}" in development | release) ;; *) exit 1 ;; esac && \
    case "${AGENT_SOURCE_COMMIT}" in "" | "<no value>" | *'{{'*) exit 1 ;; esac && \
    case "${AGENT_SOURCE_TREE_DIGEST}" in sha256:????????????????????????????????????????????????????????????????) ;; *) exit 1 ;; esac && \
    case "${AGENT_SOURCE_DIRTY}" in true | false) ;; *) exit 1 ;; esac && \
    case "${OCI_CREATED}" in "" | "<no value>") exit 1 ;; esac && \
    case "${OCI_VERSION}" in "" | "<no value>" | *'{{'*) exit 1 ;; esac
# --8<-- [end:traceability-guard]

# --8<-- [start:static-build]
# CGO_ENABLED=0 is a course invariant, not an optimization: it is what lets the binary ship
# on a base with no libc and keeps exactly one SQLite implementation in the process.
# -trimpath removes the builder's absolute paths so two machines produce the same binary;
# -s -w drop the symbol table and DWARF, which debug.ReadBuildInfo does not need.
ENV CGO_ENABLED=0 GOOS=linux
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w \
      -X github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo.buildMode=${AGENT_BUILD_MODE} \
      -X github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo.version=${OCI_VERSION} \
      -X github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo.sourceIdentity=${AGENT_SOURCE_COMMIT} \
      -X github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo.revision=${AGENT_SOURCE_REVISION} \
      -X github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo.treeDigest=${AGENT_SOURCE_TREE_DIGEST} \
      -X github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo.buildTimestamp=${OCI_CREATED} \
      -X github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo.dirty=${AGENT_SOURCE_DIRTY}" \
      -o /out/agent ./cmd/agent && \
    /out/agent version >/dev/null
# --8<-- [end:static-build]

# The runtime base has no shell, so the writable state directory is created here and
# copied in with its ownership already set.
RUN install -d -o 10001 -g 10001 /out/state

# --- Stage 2: distroless, non-root runtime -----------------------------------
# --8<-- [start:runtime-base]
FROM cgr.dev/chainguard/static@sha256:f51c2493951313c3ad4069080b2814ffb6ed6fe3909dabeb84a9482f42d5600b AS runtime
# --8<-- [end:runtime-base]

ARG AGENT_BUILD_MODE
ARG AGENT_SOURCE_COMMIT
ARG AGENT_SOURCE_REVISION
ARG AGENT_SOURCE_TREE_DIGEST
ARG AGENT_SOURCE_DIRTY
ARG OCI_CREATED
ARG OCI_VERSION
LABEL org.opencontainers.image.created="${OCI_CREATED}" \
      org.opencontainers.image.description="Executable Google ADK reference agent for the AgentOps Open Course" \
      org.opencontainers.image.documentation="https://agentops-open-course.fmind.dev/" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.revision="${AGENT_SOURCE_REVISION}" \
      org.opencontainers.image.source="https://github.com/MLOps-Courses/agentops-open-course" \
      org.opencontainers.image.title="AgentOps Agent" \
      org.opencontainers.image.version="${OCI_VERSION}" \
      dev.fmind.agentops.build-mode="${AGENT_BUILD_MODE}" \
      dev.fmind.agentops.source-dirty="${AGENT_SOURCE_DIRTY}" \
      dev.fmind.agentops.source-identity="${AGENT_SOURCE_COMMIT}" \
      dev.fmind.agentops.source-revision="${AGENT_SOURCE_REVISION}" \
      dev.fmind.agentops.source-tree-digest="${AGENT_SOURCE_TREE_DIGEST}"

ENV AGENT_DATA_DIR=/app/data \
    AGENT_STATE_DIR=/app/state \
    AGENT_A2A_BIND_HOST=0.0.0.0 \
    ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS=false \
    OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=false

WORKDIR /app

COPY --from=build /out/agent /app/agent
COPY --chown=10001:10001 data /app/data
COPY --from=build --chown=10001:10001 /out/state /app/state

USER 10001:10001
EXPOSE 8080

# ENTRYPOINT is the binary and CMD is the surface it serves, so a manifest selects the MCP
# server or the state CLI by overriding `args` alone. A `command:` override would have to
# repeat the binary path and would silently drift from it.
ENTRYPOINT ["/app/agent"]
CMD ["a2a"]
