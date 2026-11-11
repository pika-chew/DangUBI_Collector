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
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// record는 전처리된 JSONL 한 줄을 나타낸다. pld의 세부 구조는 종류별로 달라 JSON으로 보관한다.
type record struct {
	IdempotencyKey string          `json:"idempotencyKey"`
	VehicleID      string          `json:"vehicleId"`
	Timestamp      int64           `json:"ts"`
	Type           int             `json:"ty"`
	SchemaVersion  int             `json:"schemaVersion"`
	Payload        json.RawMessage `json:"pld"`
}

func main() {
	// Ctrl+C나 Pod 종료 신호를 파일 처리와 SQS 워커에 취소로 전달한다.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	// os.Args[0]은 실행 파일 이름이므로 사용자가 주는 인자는 하나만 받는다.
	if len(os.Args) != 2 {
		return errors.New("usage: collector <input.jsonl> | --worker")
	}
	db, err := openDatabase()
	if err != nil {
		return err
	}
	defer db.Close()

	if os.Args[1] == "--worker" {
		return runWorker(ctx, db)
	}
	input, err := os.Open(os.Args[1])
	if err != nil {
		return err
	}
	defer input.Close()
	return ingest(ctx, db, input)
}

// ingest는 로컬 파일과 S3에서 압축을 해제한 스트림을 같은 방식으로 읽는다.
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
		// 레코드별로 저장 시간을 제한하고, 다음 줄로 넘어가기 전에 타이머를 정리한다.
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = saveRecord(writeCtx, db, event, body)
		cancel()
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		processed++
	}
	// Scan은 입력 끝과 읽기 실패 모두에서 멈추므로 마지막에 오류를 확인한다.
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
	// 전처리 스키마 버전 1의 Trip(3)과 Microtrip(4)을 적재한다.
	if event.SchemaVersion != 1 || (event.Type != 3 && event.Type != 4) {
		return record{}, errors.New("expected schemaVersion 1 and ty 3 or 4")
	}
	if len(event.Payload) == 0 || event.Payload[0] != '{' {
		return record{}, errors.New("pld must be a JSON object")
	}
	return event, nil
}
