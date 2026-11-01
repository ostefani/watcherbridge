/**
 * JSDoc wire mirrors for the Go overview and health views.
 * Each typedef carries a `// wire:` tag naming the Go type it mirrors;
 * Step 23 components consume these instead of redeclaring shapes.
 * Nothing here is logic: field names and nullability must track the
 * Go structs exactly.
 */

// wire: overviewWire (internal/viewer/overview.go)
/**
 * @typedef {object} OverviewAttention
 * @property {string} id
 * @property {string} kind briefing|incident
 * @property {string} priority low|medium|high|critical
 * @property {string} change_kind
 * @property {string} created_at RFC 3339
 * @property {string} status attention|flagged
 */

// wire: overviewWire (internal/viewer/overview.go)
/**
 * @typedef {object} Overview
 * @property {string} verdict quiet|review|act
 * @property {string} headline
 * @property {OverviewAttention[]} attention
 * @property {Record<string, number>} digest
 * @property {number} receipts
 * @property {string} window Go duration string
 * @property {string} [last_change] RFC 3339, absent before any receipt
 * @property {string} coverage ok|degraded|blind
 * @property {string} liveness unknown until heartbeat capability exists
 * @property {SourceHealth[]} sources
 * @property {string[]} [reasons]
 */

// wire: sourceWire (internal/viewer/overview.go)
/**
 * @typedef {object} SourceHealth
 * @property {string} scope
 * @property {string} status ok|degraded|blind
 * @property {string} liveness
 * @property {string} [last_report_at] RFC 3339
 * @property {boolean} heartbeat_enabled
 * @property {string} [last_heartbeat_at] RFC 3339
 * @property {string} [delivery_status]
 * @property {string[]} reasons
 */

// wire: healthWire (internal/viewer/overview.go)
/**
 * @typedef {object} Health
 * @property {SourceHealth[]} sources
 * @property {string} coverage
 * @property {string[]} [coverage_reasons]
 * @property {{used: number, total: number, ratio: number}} fallback_1h
 * @property {number} parked_groups
 * @property {number} forward_errors_1h
 * @property {{totals: Record<string, number>, last?: object, last_success?: object}} purge
 * @property {{posted: number, dropped: number, failed: number}} [publisher]
 * @property {object} process
 * @property {number} queue_depth
 * @property {number} queue_cap
 */

export {};
