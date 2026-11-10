package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

type record struct {
	IdempotencyKey string          `json:"idempotencyKey"`
	VehicleID      string          `json:"vehicleId"`
	Timestamp      int64           `json:"ts"`
	Type           int             `json:"ty"`
	SchemaVersion  int             `json:"schemaVersion"`
	Payload        json.RawMessage `json:"pld"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: go run . <input.jsonl>")
	}
	dsn := os.Getenv("COLLECTOR_MYSQL_DSN")
	if dsn == "" {
		return errors.New("COLLECTOR_MYSQL_DSN is required")
	}
	input, err := os.Open(os.Args[1])
	if err != nil {
		return err
	}
	defer input.Close()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return errors.New("invalid COLLECTOR_MYSQL_DSN")
	}
	defer db.Close()

	return ingest(context.Background(), db, input)
}

func ingest(ctx context.Context, db *sql.DB, input io.Reader) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(nil, 1<<20)
	line := 0
	processed := 0
	for scanner.Scan() {
		line++
		body := bytes.TrimSpace(scanner.Bytes())
		if len(body) == 0 {
			continue
		}
		event, err := parseRecord(body)
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = saveRecord(writeCtx, db, event, body)
		cancel()
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		processed++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read line %d: %w", line+1, err)
	}
	log.Printf("processed %d records (including duplicates)", processed)
	return nil
}

func parseRecord(body []byte) (record, error) {
	var event record
	if err := json.Unmarshal(body, &event); err != nil {
		return record{}, fmt.Errorf("decode JSON: %w", err)
	}
	key := strings.TrimSpace(event.IdempotencyKey)
	if key == "" || key != event.IdempotencyKey || len(key) > 128 {
		return record{}, errors.New("idempotencyKey must be 1 to 128 bytes without surrounding whitespace")
	}
	if strings.TrimSpace(event.VehicleID) == "" || event.Timestamp <= 0 {
		return record{}, errors.New("vehicleId and a positive ts are required")
	}
	if event.SchemaVersion != 1 || (event.Type != 3 && event.Type != 4) {
		return record{}, errors.New("expected schemaVersion 1 and ty 3 or 4")
	}
	if len(event.Payload) == 0 || event.Payload[0] != '{' {
		return record{}, errors.New("pld must be a JSON object")
	}
	return event, nil
}
