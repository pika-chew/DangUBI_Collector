package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

func saveRecord(ctx context.Context, db *sql.DB, event record, body []byte) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO collector_events (idempotency_key, vehicle_id, event_ts, event_type, payload)
		VALUES (?, ?, ?, ?, ?)`,
		event.IdempotencyKey, event.VehicleID, event.Timestamp, event.Type, string(event.Payload),
	)
	if err != nil {
		// This insert has one unique key. A previous commit already wrote both rows.
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return nil
		}
		return fmt.Errorf("insert collector event: %w", err)
	}

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
