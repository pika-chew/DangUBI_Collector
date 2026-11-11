package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/go-sql-driver/mysql"
)

func openDatabase() (*sql.DB, error) {
	// DSN을 주면 TLS 같은 드라이버 옵션까지 지정할 수 있다. 나머지 환경 변수보다 우선한다.
	if dsn := os.Getenv("COLLECTOR_MYSQL_DSN"); dsn != "" {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			return nil, errors.New("invalid COLLECTOR_MYSQL_DSN")
		}
		return db, nil
	}

	// 주소와 계정을 드라이버에 따로 넘겨 비밀번호의 특수문자를 그대로 전달한다.
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = os.Getenv("COLLECTOR_MYSQL_ADDRESS")
	cfg.DBName = os.Getenv("COLLECTOR_MYSQL_DATABASE")
	cfg.User = os.Getenv("COLLECTOR_MYSQL_USER")
	cfg.Passwd = os.Getenv("COLLECTOR_MYSQL_PASSWORD")
	if cfg.Addr == "" || cfg.DBName == "" || cfg.User == "" || cfg.Passwd == "" {
		return nil, errors.New("set COLLECTOR_MYSQL_DSN or all of COLLECTOR_MYSQL_ADDRESS, COLLECTOR_MYSQL_DATABASE, COLLECTOR_MYSQL_USER, and COLLECTOR_MYSQL_PASSWORD")
	}
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil || host == "" {
		return nil, errors.New("COLLECTOR_MYSQL_ADDRESS must use host:port")
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, errors.New("COLLECTOR_MYSQL_ADDRESS requires a port from 1 to 65535")
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("invalid Collector MySQL configuration")
	}
	return sql.OpenDB(connector), nil
}

// saveRecord는 업무 데이터와 outbox를 한 트랜잭션으로 저장한다.
func saveRecord(ctx context.Context, db *sql.DB, event record, body []byte) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// 도중에 반환하면 롤백한다. 커밋을 마친 트랜잭션에는 영향이 없다.
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO collector_events (idempotency_key, vehicle_id, event_ts, event_type, payload)
		VALUES (?, ?, ?, ?, ?)`,
		event.IdempotencyKey, event.VehicleID, event.Timestamp, event.Type, string(event.Payload),
	)
	if err != nil {
		// 업무 테이블의 유일 키가 중복이면 이전 트랜잭션에서 outbox까지 함께 저장된 것으로 본다.
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return nil
		}
		return fmt.Errorf("insert collector event: %w", err)
	}

	// outbox에는 pld 외에 차량 ID와 이벤트 시각도 포함된 전체 레코드를 남긴다.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox (id, aggregatetype, aggregateid, type, payload)
		VALUES (?, ?, ?, ?, ?)`,
		event.IdempotencyKey, "collector_demo", event.VehicleID, "TelemetryCollected", string(body),
	)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit collector event: %w", err)
	}
	return nil
}
