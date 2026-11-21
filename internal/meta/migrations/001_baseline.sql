CREATE TABLE projects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    slug TEXT NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE project_api_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL DEFAULT 'default',
    prefix TEXT NOT NULL,
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT project_api_tokens_projects_api_tokens
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE INDEX project_api_tokens_project_id ON project_api_tokens (project_id);

CREATE TABLE source_map_upload_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    prefix TEXT NOT NULL,
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT source_map_upload_tokens_projects_source_map_upload_tokens
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE INDEX source_map_upload_tokens_project_id ON source_map_upload_tokens (project_id);

CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
    display_name TEXT NULL,
    email TEXT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    last_login_at TIMESTAMP NULL,
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE user_api_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    prefix TEXT NOT NULL,
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP NULL,
    user_id INTEGER NOT NULL,
    CONSTRAINT user_api_tokens_users_api_tokens
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE INDEX user_api_tokens_user_id ON user_api_tokens (user_id);

CREATE TABLE sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL,
    CONSTRAINT sessions_users_sessions
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE notification_channels (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    type TEXT NOT NULL CHECK (type IN ('webhook', 'feishu', 'dingtalk', 'wecom', 'smtp', 'slack')),
    name TEXT NOT NULL UNIQUE,
    config JSON NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE alert_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    signal TEXT NOT NULL DEFAULT 'metrics' CHECK (signal IN ('metrics', 'logs')),
    evaluation_engine TEXT NOT NULL DEFAULT 'native' CHECK (evaluation_engine IN ('paused', 'native')),
    expr TEXT NOT NULL,
    "for" TEXT NOT NULL DEFAULT '0s',
    severity TEXT NOT NULL DEFAULT 'warning',
    labels JSON NOT NULL,
    annotations JSON NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT alert_rules_projects_alert_rules
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE INDEX alert_rules_project_id ON alert_rules (project_id);
CREATE UNIQUE INDEX alert_rules_project_name ON alert_rules (project_id, name);

CREATE TABLE alert_rule_channels (
    alert_rule_id INTEGER NOT NULL,
    notification_channel_id INTEGER NOT NULL,
    PRIMARY KEY (alert_rule_id, notification_channel_id),
    CONSTRAINT alert_rule_channels_rule
        FOREIGN KEY (alert_rule_id) REFERENCES alert_rules (id) ON DELETE CASCADE,
    CONSTRAINT alert_rule_channels_channel
        FOREIGN KEY (notification_channel_id) REFERENCES notification_channels (id) ON DELETE CASCADE
);

CREATE TABLE silences (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    matchers JSON NOT NULL,
    starts_at TIMESTAMP NOT NULL,
    ends_at TIMESTAMP NOT NULL,
    comment TEXT NOT NULL DEFAULT '',
    creator TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT silences_projects_silences
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE INDEX silences_project_id ON silences (project_id);
CREATE INDEX silences_active_window ON silences (starts_at, ends_at);

CREATE TABLE alert_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('firing', 'resolved')),
    labels JSON NOT NULL,
    annotations JSON NOT NULL,
    starts_at TIMESTAMP NOT NULL,
    ends_at TIMESTAMP NULL,
    last_notified_at TIMESTAMP NULL,
    inhibited BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMP NOT NULL,
    rule_id INTEGER NOT NULL,
    CONSTRAINT alert_events_alert_rules_events
        FOREIGN KEY (rule_id) REFERENCES alert_rules (id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX alert_events_fingerprint_rule ON alert_events (fingerprint, rule_id);
CREATE INDEX alert_events_status ON alert_events (status);
CREATE INDEX alert_events_inhibited ON alert_events (inhibited);

CREATE TABLE notification_deliveries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id INTEGER NOT NULL,
    rule_id INTEGER NULL,
    status TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL,
    CONSTRAINT notification_deliveries_channel
        FOREIGN KEY (channel_id) REFERENCES notification_channels (id) ON DELETE CASCADE,
    CONSTRAINT notification_deliveries_rule
        FOREIGN KEY (rule_id) REFERENCES alert_rules (id) ON DELETE SET NULL
);
CREATE INDEX notification_deliveries_created_at ON notification_deliveries (created_at);

CREATE TABLE issues (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint TEXT NOT NULL,
    title TEXT NOT NULL,
    level TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'ignored')),
    count INTEGER NOT NULL DEFAULT 0,
    user_count INTEGER NOT NULL DEFAULT 0,
    user_bitmap INTEGER NOT NULL DEFAULT 0,
    first_seen TIMESTAMP NOT NULL,
    last_seen TIMESTAMP NOT NULL,
    status_changed_at TIMESTAMP NOT NULL,
    regressed_at TIMESTAMP NULL,
    last_event_id TEXT NOT NULL,
    ai_analysis JSON NULL,
    ai_analysis_at TIMESTAMP NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT issues_projects_issues
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE UNIQUE INDEX issues_project_fingerprint ON issues (project_id, fingerprint);
CREATE INDEX issues_project_status_last_seen ON issues (project_id, status, last_seen);

CREATE TABLE telemetry_inbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL,
    canonical_digest TEXT NOT NULL,
    payload BLOB NULL,
    payload_bytes INTEGER NOT NULL CHECK (payload_bytes >= 0),
    occurred_at TIMESTAMP NOT NULL,
    accepted_at TIMESTAMP NOT NULL,
    source_map_snapshot JSON NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'terminal')),
    available_at TIMESTAMP NOT NULL,
    claim_owner TEXT NOT NULL DEFAULT '',
    claim_until TIMESTAMP NULL,
    claim_epoch INTEGER NOT NULL DEFAULT 0 CHECK (claim_epoch >= 0),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT '',
    processed_at TIMESTAMP NULL,
    completed_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT telemetry_inbox_project
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE UNIQUE INDEX telemetry_inbox_project_event ON telemetry_inbox (project_id, event_id);
CREATE INDEX telemetry_inbox_due ON telemetry_inbox (status, available_at, id);
CREATE INDEX telemetry_inbox_status_accepted_at ON telemetry_inbox (status, accepted_at, id);
CREATE INDEX telemetry_inbox_claim_until ON telemetry_inbox (claim_until);
CREATE INDEX telemetry_inbox_completed_at ON telemetry_inbox (completed_at);

CREATE TABLE issue_lifecycle_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER NOT NULL,
    issue_id INTEGER NOT NULL,
    inbox_item_id INTEGER NOT NULL CHECK (inbox_item_id >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('new_issue', 'regression')),
    title TEXT NOT NULL,
    level TEXT NOT NULL,
    seen_at TIMESTAMP NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'dispatched', 'failed')),
    available_at TIMESTAMP NOT NULL,
    claim_owner TEXT NOT NULL DEFAULT '',
    claim_until TIMESTAMP NULL,
    claim_epoch INTEGER NOT NULL DEFAULT 0 CHECK (claim_epoch >= 0),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    dispatched_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    CONSTRAINT issue_lifecycle_events_project
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE INDEX issue_lifecycle_events_due ON issue_lifecycle_events (status, available_at, id);
CREATE INDEX issue_lifecycle_events_claim_until ON issue_lifecycle_events (claim_until);
CREATE INDEX issue_lifecycle_events_project_created ON issue_lifecycle_events (project_id, created_at);
CREATE INDEX issue_lifecycle_events_dispatched_at ON issue_lifecycle_events (dispatched_at);

CREATE TABLE telemetry_inbox_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    active_items INTEGER NOT NULL DEFAULT 0 CHECK (active_items >= 0),
    active_bytes INTEGER NOT NULL DEFAULT 0 CHECK (active_bytes >= 0),
    rejections_total INTEGER NOT NULL DEFAULT 0 CHECK (rejections_total >= 0),
    retries_total INTEGER NOT NULL DEFAULT 0 CHECK (retries_total >= 0),
    terminal_failures_total INTEGER NOT NULL DEFAULT 0 CHECK (terminal_failures_total >= 0),
    drops_total INTEGER NOT NULL DEFAULT 0 CHECK (drops_total >= 0),
    updated_at TIMESTAMP NOT NULL
);
INSERT INTO telemetry_inbox_state(id, updated_at) VALUES (1, CURRENT_TIMESTAMP);

CREATE TABLE replay_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    started_at TIMESTAMP NOT NULL,
    last_seen_at TIMESTAMP NOT NULL,
    segment_count INTEGER NOT NULL DEFAULT 0 CHECK (segment_count >= 0),
    total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    has_error BOOLEAN NOT NULL DEFAULT FALSE,
    release TEXT NULL,
    page_url TEXT NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT replay_sessions_projects_replay_sessions
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE UNIQUE INDEX replay_sessions_project_session ON replay_sessions (project_id, session_id);
CREATE INDEX replay_sessions_project_last_seen ON replay_sessions (project_id, last_seen_at);

CREATE TABLE replay_segments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 0),
    started_at TIMESTAMP NOT NULL,
    last_seen_at TIMESTAMP NOT NULL,
    event_count INTEGER NOT NULL DEFAULT 0 CHECK (event_count >= 0),
    size_bytes INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    payload_sha256 TEXT NOT NULL DEFAULT '',
    markers_json TEXT NOT NULL DEFAULT '[]',
    created_at TIMESTAMP NOT NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT replay_segments_projects_replay_segments
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE UNIQUE INDEX replay_segments_project_session_seq
    ON replay_segments (project_id, session_id, seq);

CREATE TABLE source_map_artifacts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    release TEXT NOT NULL,
    filename TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    sha256 TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT source_map_artifacts_projects_source_map_artifacts
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE UNIQUE INDEX source_map_artifacts_project_release_filename
    ON source_map_artifacts (project_id, release, filename);
CREATE INDEX source_map_artifacts_project_created_at
    ON source_map_artifacts (project_id, created_at);

CREATE TABLE project_alert_configs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    new_issue BOOLEAN NOT NULL DEFAULT TRUE,
    regression BOOLEAN NOT NULL DEFAULT TRUE,
    error_rate_threshold_per_min INTEGER NOT NULL DEFAULT 0 CHECK (error_rate_threshold_per_min >= 0),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    project_id INTEGER NOT NULL UNIQUE,
    CONSTRAINT project_alert_configs_projects_alert_config
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);

CREATE TABLE project_alert_config_channels (
    project_alert_config_id INTEGER NOT NULL,
    notification_channel_id INTEGER NOT NULL,
    PRIMARY KEY (project_alert_config_id, notification_channel_id),
    CONSTRAINT project_alert_config_channels_config
        FOREIGN KEY (project_alert_config_id) REFERENCES project_alert_configs (id) ON DELETE CASCADE,
    CONSTRAINT project_alert_config_channels_channel
        FOREIGN KEY (notification_channel_id) REFERENCES notification_channels (id) ON DELETE CASCADE
);

CREATE TABLE scrape_targets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    interval TEXT NOT NULL DEFAULT '30s',
    labels JSON NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT scrape_targets_projects_scrape_targets
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE UNIQUE INDEX scrape_target_project_id_name ON scrape_targets (project_id, name);

CREATE TABLE inhibition_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_matchers JSON NOT NULL,
    target_matchers JSON NOT NULL,
    equal_labels JSON NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    comment TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    project_id INTEGER NOT NULL,
    CONSTRAINT inhibition_rules_projects_inhibition_rules
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE INDEX inhibition_rules_project_enabled ON inhibition_rules (project_id, enabled);

CREATE TABLE profile_metas (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    upload_id TEXT NOT NULL,
    payload_sha256 TEXT NOT NULL,
    service TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('cpu', 'heap', 'goroutine')),
    start TIMESTAMP NOT NULL,
    end TIMESTAMP NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    blob_key TEXT NOT NULL UNIQUE,
    labels JSON NOT NULL DEFAULT '{}',
    project_id INTEGER NOT NULL,
    CONSTRAINT profile_metas_projects_profile_metas
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE NO ACTION
);
CREATE UNIQUE INDEX profile_metas_project_upload_id
    ON profile_metas (project_id, upload_id);
CREATE INDEX profile_metas_project_service_type_start
    ON profile_metas (project_id, service, type, start);
CREATE INDEX profile_metas_project_end ON profile_metas (project_id, end);

CREATE TABLE alert_engine_members (
    member_id TEXT PRIMARY KEY,
    started_at TIMESTAMP NOT NULL,
    heartbeat_at TIMESTAMP NOT NULL,
    expires_at TIMESTAMP NOT NULL
);
CREATE INDEX alert_engine_members_expires_at ON alert_engine_members (expires_at);

CREATE TABLE alert_rule_leases (
    rule_id INTEGER PRIMARY KEY,
    owner_id TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    CONSTRAINT alert_rule_leases_rule
        FOREIGN KEY (rule_id) REFERENCES alert_rules (id) ON DELETE CASCADE
);
CREATE INDEX alert_rule_leases_expires_at ON alert_rule_leases (expires_at);

CREATE TABLE alert_rule_states (
    rule_id INTEGER NOT NULL,
    fingerprint TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'firing', 'recovered')),
    pending_since TIMESTAMP NULL,
    firing_since TIMESTAMP NULL,
    last_evaluated_at TIMESTAMP NOT NULL,
    labels JSON NOT NULL,
    annotations JSON NOT NULL,
    value REAL NULL,
    lease_epoch INTEGER NOT NULL,
    last_notified_at TIMESTAMP NULL,
    PRIMARY KEY (rule_id, fingerprint),
    CONSTRAINT alert_rule_states_rule
        FOREIGN KEY (rule_id) REFERENCES alert_rules (id) ON DELETE CASCADE
);
CREATE INDEX alert_rule_states_status ON alert_rule_states (status);

CREATE TABLE notification_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    idempotency_key TEXT NOT NULL UNIQUE,
    project_id INTEGER NOT NULL,
    rule_id INTEGER NULL,
    channel_id INTEGER NOT NULL,
    payload JSON NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'delivered', 'dead')),
    available_at TIMESTAMP NOT NULL,
    claim_owner TEXT NULL,
    claim_until TIMESTAMP NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    delivered_at TIMESTAMP NULL,
    CONSTRAINT notification_outbox_project
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
    CONSTRAINT notification_outbox_rule
        FOREIGN KEY (rule_id) REFERENCES alert_rules (id) ON DELETE SET NULL,
    CONSTRAINT notification_outbox_channel
        FOREIGN KEY (channel_id) REFERENCES notification_channels (id) ON DELETE CASCADE
);
CREATE INDEX notification_outbox_due
    ON notification_outbox (status, available_at, priority DESC, id);
CREATE INDEX notification_outbox_claim_until ON notification_outbox (claim_until);
CREATE INDEX notification_outbox_status_updated ON notification_outbox (status, updated_at);

CREATE TABLE audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    action TEXT NOT NULL,
    actor_id INTEGER NULL,
    actor_username TEXT NOT NULL DEFAULT 'anonymous',
    actor_role TEXT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id INTEGER NULL,
    target_name TEXT NOT NULL DEFAULT '',
    target_role TEXT NOT NULL DEFAULT '',
    project_id INTEGER NULL,
    project_slug TEXT NOT NULL DEFAULT '',
    changed_fields TEXT NOT NULL DEFAULT '',
    success BOOLEAN NOT NULL DEFAULT TRUE,
    failure_reason TEXT NOT NULL DEFAULT '',
    http_method TEXT NOT NULL DEFAULT '',
    http_path TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL
);
CREATE INDEX audit_events_created_at ON audit_events (created_at);
CREATE INDEX audit_events_actor_created ON audit_events (actor_username, created_at);
CREATE INDEX audit_events_action_created ON audit_events (action, created_at);
CREATE INDEX audit_events_project_created ON audit_events (project_id, created_at);

CREATE TABLE ai_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    provider TEXT NOT NULL DEFAULT '',
    base_url TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    api_key_ciphertext TEXT NOT NULL DEFAULT '',
    max_tokens INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE platform_key_fingerprints (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    fingerprint TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE platform_error_rate_buckets (
    project_id INTEGER NOT NULL,
    bucket_epoch INTEGER NOT NULL,
    count INTEGER NOT NULL DEFAULT 0 CHECK (count >= 0),
    PRIMARY KEY (project_id, bucket_epoch),
    CONSTRAINT platform_error_rate_buckets_project
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);

CREATE TABLE platform_alert_states (
    project_id INTEGER NOT NULL,
    kind TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('firing', 'resolved')),
    active_cycle INTEGER NOT NULL DEFAULT 0 CHECK (active_cycle >= 0),
    starts_at TIMESTAMP NULL,
    ends_at TIMESTAMP NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (project_id, kind),
    CONSTRAINT platform_alert_states_project
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);
CREATE INDEX platform_alert_states_kind_status ON platform_alert_states (kind, status);

CREATE TABLE deleted_project_slugs (
    slug TEXT PRIMARY KEY,
    deleted_at TIMESTAMP NOT NULL,
    blobs_cleaned_at TIMESTAMP NULL
);
CREATE INDEX deleted_project_slugs_blobs_cleaned ON deleted_project_slugs (blobs_cleaned_at);

CREATE TABLE source_map_upload_pins (
    token TEXT NOT NULL,
    project_id INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    PRIMARY KEY (token, sha256),
    CONSTRAINT source_map_upload_pins_project
        FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);
CREATE INDEX source_map_upload_pins_project_expires_at
    ON source_map_upload_pins (project_id, expires_at);
CREATE INDEX source_map_upload_pins_expires_at
    ON source_map_upload_pins (expires_at);
