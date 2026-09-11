package domain

// pivotDomain is a second, deliberately unrelated vocabulary.
//
// It exists so the portability seam can be exercised without maintaining a
// second application: anything that works for both this and [Reference] is
// reading identifiers through the seam, and anything that works for only one of
// them has a seed identifier baked in somewhere.
//
// It is a function, not a var, for the same reason [Reference] is.
func pivotDomain() Vocabulary {
	return Vocabulary{
		Incidents: IncidentVocabulary{
			CheckoutLatency: "INC-101",
			InventoryDown:   "INC-102",
			PaymentsErrors:  "INC-103",
			AuthErrors:      "INC-104",
			SearchLatency:   "INC-105",
			CheckoutDisk:    "INC-106",
			CacheMemory:     "INC-107",
			DatabaseCascade: "INC-108",
			CheckoutCascade: "INC-109",
			GatewayMemory:   "INC-110",
		},
		Services: ServiceVocabulary{
			Checkout:  "orders",
			Payments:  "billing",
			Auth:      "identity",
			Search:    "catalog-search",
			Inventory: "catalog",
			Database:  "datastore",
			Cache:     "redis",
			Gateway:   "edge",
		},
		Runbooks: RunbookVocabulary{
			CascadeFailure:     "dependency-cascade",
			DeploymentRollback: "release-rollback",
			DiskFull:           "storage-full",
			ElevatedErrors:     "error-spike",
			HighLatency:        "slow-requests",
			MemoryLeak:         "heap-growth",
			ServiceDown:        "service-unavailable",
		},
		DependencyEdges: []DependencyEdge{
			{From: "redis", To: "datastore"},
			{From: "datastore", To: "orders"},
			{From: "catalog", To: "orders"},
		},
	}
}
