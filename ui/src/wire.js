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
 * @property {Record<string, number>} [explained] explicitly-expected briefings per kind
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
 * @property {string} [identity_namespace] absent until Step 21 reports it
 * @property {string} [capabilities] absent until Step 21 reports it
 * @property {string} [last_report_at] RFC 3339
 * @property {boolean} heartbeat_enabled heartbeat opt-in, not identity proof
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
 * @property {{reports_seconds: number, subjects_seconds?: number}} [retention] policy, never a per-profile boundary
 * @property {{posted: number, dropped: number, failed: number}} [publisher]
 * @property {object} process
 * @property {number} queue_depth
 * @property {number} queue_cap
 */

// wire: decisionWire + reviewWire (internal/viewer/decisions.go)
/**
 * @typedef {object} Decision
 * @property {string} decision_id
 * @property {string} subject_kind briefing|reported_subject
 * @property {string} subject_id
 * @property {string} kind expected|not_expected|snooze|note|label|undo
 * @property {string} [until] RFC 3339, snooze only
 * @property {string} [text] note|label only
 * @property {string} created_at RFC 3339
 * @property {string} [supersedes] undo target
 * @property {boolean} undone
 * @property {string} cursor "<created nanos>:<rowid>" paging state, never identity
 */

/**
 * @typedef {object} EffectiveReview
 * @property {string} kind expected|not_expected|snooze
 * @property {string} [until] RFC 3339, snooze only
 * @property {string} decision_id
 */

// wire: briefingView (internal/viewer/viewer.go)
/**
 * @typedef {object} BriefingDetail
 * @property {EffectiveReview} [effective_review] absent without live review
 * @property {Decision[]} decisions newest-first audit trail
 * @property {string} briefing_id
 * @property {string} group_id
 * @property {string} host
 * @property {string} baseline
 * @property {string|null} model_suggestion
 * @property {string} priority
 * @property {string} priority_reason
 * @property {string} baseline_rule
 * @property {string} kind
 * @property {string} source rules|model|agent|fallback|legacy
 * @property {boolean} clamped
 * @property {boolean} fallback_used
 * @property {string} what_changed
 * @property {string} [likely_explanation] agent/model explanation only
 * @property {string} [confidence] agent/model confidence label only
 * @property {Array<{event_id?: string, tool_step?: number}>} [evidence_refs] citations only
 * @property {number} [input_tokens] producing-call accounting only
 * @property {number} [output_tokens] producing-call accounting only
 * @property {number} [cost_usd] producing-call accounting only, absent while unknown
 * @property {Array<{step: number, tool: string, args: string, result: string, status: string, duration_ms: number}>} [traces] executed tool steps, detail fetch only
 * @property {string} context retained reported context, not raw evidence
 * @property {string} recommended_check text only, never an action
 * @property {string} created_at RFC 3339
 * @property {string[]} event_ids
 * @property {number} seq
 * @property {{profile_id: string, role: string}[]} profiles persisted links only
 * @property {boolean} [profiles_truncated]
 * @property {{event_id: string, event_type: string}[]} referenced_events index, not evidence
 * @property {boolean} [referenced_truncated]
 * @property {Observation[]} observations retained views for exactly those events
 * @property {boolean} [observations_truncated]
 */

// wire: deviceWire (internal/viewer/viewer.go)
/**
 * @typedef {object} Observation
 * @property {number} observation_id
 * @property {string} event_id
 * @property {string} view_role
 * @property {string} reported_at RFC 3339
 * @property {string} received_at RFC 3339
 * @property {Record<string, any>} attributes
 * @property {Array<{proto: string, port: number, state: string, service: string}>} [ports] latest evidence only
 * @property {number} ports_total
 * @property {boolean} ports_truncated
 */

// wire: deviceWire (internal/viewer/viewer.go)
/**
 * @typedef {object} DeviceProfile
 * @property {string} profile_id
 * @property {string} class reported_device|address|event|legacy
 * @property {string} source_scope
 * @property {string} namespace
 * @property {string} reported_id
 * @property {string} reported_source
 * @property {string} reported_confidence
 * @property {string} association_status
 * @property {Record<string, number>} associations
 * @property {string|null} operator_label your label, never identity
 * @property {string} first_report_at RFC 3339
 * @property {string} last_report_at RFC 3339
 * @property {Observation|null} latest
 * @property {Observation[]} [history]
 * @property {string} [history_next_after]
 * @property {Decision[]} [decisions] newest-first audit trail
 */

// wire: devicesEnvelope (internal/viewer/viewer.go)
/**
 * @typedef {object} DevicesPage
 * @property {DeviceProfile[]} devices
 * @property {string} next_after "" when exhausted
 */

export {};
