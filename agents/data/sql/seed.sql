-- AgentOps Open Course — deterministic seed data for the AgentOps Agent dataset.
-- Fixed rows so tools, RAG, and evals are reproducible offline. Times are ISO-8601 UTC.

INSERT INTO services (name, description, status, owner) VALUES
    ('checkout',  'Checkout and shopping-cart service',      'degraded',    'team-payments'),
    ('payments',  'Payment processing gateway',              'operational', 'team-payments'),
    ('auth',      'Authentication and identity service',     'operational', 'team-platform'),
    ('search',    'Product search API',                      'operational', 'team-discovery'),
    ('inventory', 'Inventory and stock-availability service', 'down',       'team-fulfillment'),
    ('database',  'Primary transactional database cluster',   'operational', 'team-platform'),
    ('cache',     'Shared read-through cache fleet',          'operational', 'team-platform'),
    ('api-gateway', 'Public API gateway and edge routing',    'degraded',    'team-platform');

INSERT INTO incidents (id, service, title, severity, status, runbook, opened_at, resolved_at, summary) VALUES
    ('INC-001', 'checkout',  'Checkout latency spike',        'SEV2', 'investigating', 'high-latency',
        '2026-07-05T08:15:00Z', NULL,
        'p99 checkout latency rose from 200ms to 3.5s right after the 08:00 deploy.'),
    ('INC-002', 'inventory', 'Inventory service unavailable', 'SEV1', 'open',          'service-down',
        '2026-07-05T09:02:00Z', NULL,
        'Inventory pods are crash-looping; stock lookups return HTTP 503.'),
    ('INC-003', 'payments',  'Elevated payment declines',     'SEV3', 'resolved',      'elevated-errors',
        '2026-07-04T14:20:00Z', '2026-07-04T15:05:00Z',
        'Decline rate hit 12% due to an expired downstream certificate; certificate rotated.'),
    ('INC-004', 'auth',      'Token validation errors',       'SEV2', 'resolved',      'elevated-errors',
        '2026-07-03T22:10:00Z', '2026-07-03T23:00:00Z',
        '5xx from token validation caused by an exhausted connection pool; pool size raised.'),
    ('INC-005', 'search',    'Search results slow',           'SEV3', 'open',          'high-latency',
        '2026-07-05T07:40:00Z', NULL,
        'Search p95 degraded after an index rebuild; cache hit-rate dropped to 40%.'),
    ('INC-006', 'checkout',  'Disk full on checkout node',    'SEV2', 'resolved',      'disk-full',
        '2026-07-02T03:30:00Z', '2026-07-02T04:15:00Z',
        'Log volume filled the disk; rotated logs and expanded the volume.'),
    -- INC-007..INC-009 form one cascading failure: the cache eviction storm (root cause)
    -- overloaded the database, which in turn slowed checkout. Diagnose upstream-first.
    ('INC-007', 'cache',     'Cache eviction storm',          'SEV2', 'resolved',      'memory-leak',
        '2026-07-07T09:48:00Z', '2026-07-07T12:30:00Z',
        'A leaking serializer buffer exhausted cache memory and triggered mass evictions; hit-rate fell from 92% to 31%. Root cause of INC-008 and INC-009; fixed by deploying v2026.07.07-2.'),
    ('INC-008', 'database',  'Database connection saturation', 'SEV1', 'resolved',     'cascade-failure',
        '2026-07-07T10:12:00Z', '2026-07-07T12:45:00Z',
        'Cache misses from INC-007 multiplied read load eightfold and exhausted the connection pool (200/200). Restarting the database did not help; recovered once the cache (INC-007) was fixed.'),
    ('INC-009', 'checkout',  'Checkout latency cascade',      'SEV2', 'investigating', 'cascade-failure',
        '2026-07-07T10:20:00Z', NULL,
        'Checkout p99 reached 4.8s while queries queued on the saturated database (INC-008), itself a symptom of the cache eviction storm (INC-007). Latency is recovering; verifying baseline.'),
    -- Deliberately ambiguous: a slow leak and rising latency both fit, so retrieval
    -- evals can score whether the agent weighs memory-leak against high-latency.
    ('INC-010', 'api-gateway', 'Gateway workers degrade over uptime', 'SEV3', 'open',  'memory-leak',
        '2026-07-08T06:30:00Z', NULL,
        'Gateway worker RSS climbs about 80MB per hour and p99 latency doubles before each scheduled restart clears it; unclear whether a slow leak or a slow upstream dependency is primary.');
