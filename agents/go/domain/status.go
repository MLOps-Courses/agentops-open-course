package domain

import "fmt"

// IncidentStatus is an incident lifecycle state. The values are exactly the
// ones the SQLite CHECK constraint admits, so a parsed status is always
// storable and a stored status is always parseable.
type IncidentStatus string

const (
	IncidentStatusOpen          IncidentStatus = "open"
	IncidentStatusInvestigating IncidentStatus = "investigating"
	IncidentStatusResolved      IncidentStatus = "resolved"
)

// ParseIncidentStatus turns untrusted text into an [IncidentStatus].
func ParseIncidentStatus(value string) (IncidentStatus, error) {
	switch status := IncidentStatus(value); status {
	case IncidentStatusOpen, IncidentStatusInvestigating, IncidentStatusResolved:
		return status, nil
	default:
		return "", fmt.Errorf(
			"%w: %q is not an incident status (%s, %s, %s)",
			ErrInvalid, value,
			IncidentStatusOpen, IncidentStatusInvestigating, IncidentStatusResolved,
		)
	}
}

// Severity is an incident severity. The names order by their numeric suffix,
// but no ordering is exposed: the Python source documented the ordering and
// deliberately did not implement a comparison, and inventing one here would be
// a new contract for callers to depend on.
type Severity string

const (
	SeveritySev1 Severity = "SEV1"
	SeveritySev2 Severity = "SEV2"
	SeveritySev3 Severity = "SEV3"
)

// ParseSeverity turns untrusted text into a [Severity].
func ParseSeverity(value string) (Severity, error) {
	switch severity := Severity(value); severity {
	case SeveritySev1, SeveritySev2, SeveritySev3:
		return severity, nil
	default:
		return "", fmt.Errorf(
			"%w: %q is not a severity (%s, %s, %s)",
			ErrInvalid, value, SeveritySev1, SeveritySev2, SeveritySev3,
		)
	}
}

// ServiceStatus is a service health state, again mirroring the SQLite CHECK
// constraint.
type ServiceStatus string

const (
	ServiceStatusOperational ServiceStatus = "operational"
	ServiceStatusDegraded    ServiceStatus = "degraded"
	ServiceStatusDown        ServiceStatus = "down"
)

// ParseServiceStatus turns untrusted text into a [ServiceStatus].
func ParseServiceStatus(value string) (ServiceStatus, error) {
	switch status := ServiceStatus(value); status {
	case ServiceStatusOperational, ServiceStatusDegraded, ServiceStatusDown:
		return status, nil
	default:
		return "", fmt.Errorf(
			"%w: %q is not a service status (%s, %s, %s)",
			ErrInvalid, value,
			ServiceStatusOperational, ServiceStatusDegraded, ServiceStatusDown,
		)
	}
}
