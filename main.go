package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Ctrl+C나 Pod 종료 신호를 SQS 수신과 파일 복사에 전달한다.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	if len(os.Args) != 2 || os.Args[1] != "--worker" {
		return errors.New("usage: collector --worker")
	}
	return runWorker(ctx)
}
