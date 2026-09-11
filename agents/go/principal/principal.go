// Package principal carries authenticated caller provenance from a network
// boundary to the code that authorizes state changes.
package principal

import (
	"context"
	"errors"
	"strings"
)

const maxSubjectBytes = 255

// NetworkState distinguishes a local in-process call from authenticated and
// unauthenticated network calls. A non-empty user-shaped string is deliberately
// insufficient: only the network boundary can bind NetworkAuthenticated.
type NetworkState uint8

const (
	NotNetwork NetworkState = iota
	NetworkUnauthenticated
	NetworkAuthenticated
)

// Authenticated is a subject validated by the trusted network boundary. Its
// field is private so callers must use [NewAuthenticated].
type Authenticated struct {
	subject string
}

// NewAuthenticated validates and normalizes an authenticated subject.
func NewAuthenticated(raw string) (Authenticated, error) {
	subject := strings.TrimSpace(raw)
	if subject == "" {
		return Authenticated{}, errors.New("authenticated subject is empty")
	}
	if len(subject) > maxSubjectBytes {
		return Authenticated{}, errors.New("authenticated subject is too long")
	}
	for index := range len(subject) {
		if subject[index] < '!' || subject[index] > '~' {
			// The gateway forwards an OIDC subject identifier, not display text.
			// A bounded visible-ASCII contract keeps its use in indexes, session
			// keys, audit rows, and logs deterministic without truncating identity.
			return Authenticated{}, errors.New("authenticated subject must use visible ASCII")
		}
	}
	return Authenticated{subject: subject}, nil
}

// Subject returns the normalized authenticated subject.
func (p Authenticated) Subject() string { return p.subject }

type networkBoundary struct {
	principal Authenticated
}

type networkKey struct{}

// MarkNetwork records that ctx came from a network boundary without granting it
// an authenticated principal.
func MarkNetwork(ctx context.Context) context.Context {
	return context.WithValue(ctx, networkKey{}, networkBoundary{})
}

// BindNetwork records the principal authenticated by the network boundary. A
// zero value fails closed as unauthenticated even if it is passed accidentally.
func BindNetwork(ctx context.Context, authenticated Authenticated) context.Context {
	return context.WithValue(ctx, networkKey{}, networkBoundary{principal: authenticated})
}

// Network returns the authenticated principal, if any, and the explicit state
// of the network boundary carried by ctx.
func Network(ctx context.Context) (Authenticated, NetworkState) {
	boundary, ok := ctx.Value(networkKey{}).(networkBoundary)
	if !ok {
		return Authenticated{}, NotNetwork
	}
	if boundary.principal.subject == "" {
		return Authenticated{}, NetworkUnauthenticated
	}
	return boundary.principal, NetworkAuthenticated
}
