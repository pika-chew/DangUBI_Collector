CREATE TABLE IF NOT EXISTS collector_events (
    idempotency_key VARBINARY(128) NOT NULL PRIMARY KEY,
    vehicle_id VARCHAR(128) NOT NULL,
    event_ts BIGINT NOT NULL,
    event_type TINYINT UNSIGNED NOT NULL,
    payload JSON NOT NULL
) ENGINE = InnoDB;

CREATE TABLE IF NOT EXISTS outbox (
    id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL PRIMARY KEY,
    aggregatetype VARCHAR(64) NOT NULL,
    aggregateid VARCHAR(128) NOT NULL,
    type VARCHAR(64) NOT NULL,
    payload JSON NOT NULL
) ENGINE = InnoDB;
