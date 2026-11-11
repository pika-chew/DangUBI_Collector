CREATE TABLE IF NOT EXISTS collector_events (
    -- 기본 키 제약으로 여러 워커가 동시에 같은 레코드를 받아도 한 번만 저장한다.
    idempotency_key VARBINARY(128) NOT NULL PRIMARY KEY,
    vehicle_id VARCHAR(128) NOT NULL,
    event_ts BIGINT NOT NULL,
    event_type TINYINT UNSIGNED NOT NULL,
    payload JSON NOT NULL
) ENGINE = InnoDB;

-- 검증용 outbox는 Debezium Event Router가 읽는 컬럼 이름을 사용한다.
CREATE TABLE IF NOT EXISTS outbox (
    id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL PRIMARY KEY,
    aggregatetype VARCHAR(64) NOT NULL,
    aggregateid VARCHAR(128) NOT NULL,
    type VARCHAR(64) NOT NULL,
    payload JSON NOT NULL
) ENGINE = InnoDB;
