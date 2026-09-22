CREATE SCHEMA IF NOT EXISTS vkt7;

CREATE TABLE IF NOT EXISTS vkt7.devices (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    address SMALLINT NOT NULL,
    serial_port TEXT,
    baud_rate INTEGER,
    interface TEXT NOT NULL DEFAULT 'rs232',
    firmware_version SMALLINT,
    scheme_tv1 INTEGER,
    scheme_tv2 INTEGER,
    subscriber_id TEXT,
    report_day SMALLINT,
    model SMALLINT,
    server_version SMALLINT,
    active_db SMALLINT,
    last_seen_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS vkt7.properties (
    device_id BIGINT NOT NULL REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    element_address INTEGER NOT NULL,
    name TEXT NOT NULL,
    value_text TEXT,
    numeric_value DOUBLE PRECISION,
    raw BYTEA,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(device_id, element_address)
);

CREATE TABLE IF NOT EXISTS vkt7.active_elements (
    device_id BIGINT NOT NULL REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    element_address INTEGER NOT NULL,
    element_size INTEGER NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(device_id, element_address)
);

CREATE TABLE IF NOT EXISTS vkt7.hourly_archive (
    device_id BIGINT NOT NULL REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    archive_time TIMESTAMP NOT NULL,
    scheme_tv1 INTEGER,
    scheme_tv2 INTEGER,
    active_db SMALLINT,
    "values" JSONB NOT NULL DEFAULT '{}',
    quality JSONB NOT NULL DEFAULT '{}',
    ns JSONB NOT NULL DEFAULT '{}',
    raw JSONB,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(device_id, archive_time)
);
CREATE INDEX IF NOT EXISTS hourly_archive_time_idx ON vkt7.hourly_archive(device_id, archive_time DESC);

CREATE TABLE IF NOT EXISTS vkt7.daily_archive (
    device_id BIGINT NOT NULL REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    archive_date DATE NOT NULL,
    scheme_tv1 INTEGER,
    scheme_tv2 INTEGER,
    active_db SMALLINT,
    "values" JSONB NOT NULL DEFAULT '{}',
    quality JSONB NOT NULL DEFAULT '{}',
    ns JSONB NOT NULL DEFAULT '{}',
    raw JSONB,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(device_id, archive_date)
);
CREATE INDEX IF NOT EXISTS daily_archive_date_idx ON vkt7.daily_archive(device_id, archive_date DESC);

CREATE TABLE IF NOT EXISTS vkt7.monthly_archive (
    device_id BIGINT NOT NULL REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    archive_date DATE NOT NULL,
    scheme_tv1 INTEGER,
    scheme_tv2 INTEGER,
    active_db SMALLINT,
    "values" JSONB NOT NULL DEFAULT '{}',
    quality JSONB NOT NULL DEFAULT '{}',
    ns JSONB NOT NULL DEFAULT '{}',
    raw JSONB,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(device_id, archive_date)
);
CREATE INDEX IF NOT EXISTS monthly_archive_date_idx ON vkt7.monthly_archive(device_id, archive_date DESC);

CREATE TABLE IF NOT EXISTS vkt7.total_archive (
    device_id BIGINT NOT NULL REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    archive_date DATE NOT NULL,
    scheme_tv1 INTEGER,
    scheme_tv2 INTEGER,
    active_db SMALLINT,
    "values" JSONB NOT NULL DEFAULT '{}',
    quality JSONB NOT NULL DEFAULT '{}',
    ns JSONB NOT NULL DEFAULT '{}',
    raw JSONB,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(device_id, archive_date)
);
CREATE INDEX IF NOT EXISTS total_archive_date_idx ON vkt7.total_archive(device_id, archive_date DESC);

CREATE TABLE IF NOT EXISTS vkt7.current_values (
    id BIGSERIAL PRIMARY KEY,
    device_id BIGINT NOT NULL REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    "values" JSONB NOT NULL DEFAULT '{}',
    quality JSONB NOT NULL DEFAULT '{}',
    ns JSONB NOT NULL DEFAULT '{}',
    raw JSONB
);
CREATE INDEX IF NOT EXISTS current_values_idx ON vkt7.current_values(device_id, received_at DESC);

CREATE TABLE IF NOT EXISTS vkt7.current_totals (
    id BIGSERIAL PRIMARY KEY,
    device_id BIGINT NOT NULL REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    "values" JSONB NOT NULL DEFAULT '{}',
    quality JSONB NOT NULL DEFAULT '{}',
    ns JSONB NOT NULL DEFAULT '{}',
    raw JSONB
);
CREATE INDEX IF NOT EXISTS current_totals_idx ON vkt7.current_totals(device_id, received_at DESC);

CREATE TABLE IF NOT EXISTS vkt7.collection_log (
    id BIGSERIAL PRIMARY KEY,
    device_id BIGINT REFERENCES vkt7.devices(id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    status TEXT NOT NULL,
    error TEXT
);
