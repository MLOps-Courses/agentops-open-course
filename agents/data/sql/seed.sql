-- AgentOps Open Course — deterministic seed data for the Ops Copilot dataset.
-- Fixed rows so tools, RAG, and evals are reproducible offline. Times are ISO-8601 UTC.

INSERT INTO services (name, description, status, owner) VALUES
    ('checkout',  'Checkout and shopping-cart service',      'degraded',    'team-payments'),
    ('payments',  'Payment processing gateway',              'operational', 'team-payments'),
    ('auth',      'Authentication and identity service',     'operational', 'team-platform'),
    ('search',    'Product search API',                      'operational', 'team-discovery'),
    ('inventory', 'Inventory and stock-availability service', 'down',       'team-fulfillment');

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
        'Log volume filled the disk; rotated logs and expanded the volume.');
