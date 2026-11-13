package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const (
	// 120초 숨김 시간이 끝나기 전에 60초마다 연장한다.
	visibilitySeconds = 120
	visibilityRefresh = 60 * time.Second
	// SQS가 메시지를 기다리는 시간보다 HTTP 호출 제한을 길게 둔다.
	pollWaitSeconds  = 20
	pollCallTimeout  = 30 * time.Second
	queueCallTimeout = 10 * time.Second
)

// queueClient는 워커에 필요한 SQS 동작만 정의해 테스트에서 모의 클라이언트를 쓸 수 있게 한다.
type queueClient interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type queueWorker struct {
	queue    queueClient
	queueURL string
	input    s3Input
	refresh  time.Duration
}

func runWorker(ctx context.Context) error {
	queueURL := os.Getenv("COLLECTOR_SQS_QUEUE_URL")
	bucket := os.Getenv("COLLECTOR_S3_BUCKET")
	if queueURL == "" || bucket == "" {
		return errors.New("COLLECTOR_SQS_QUEUE_URL and COLLECTOR_S3_BUCKET are required")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return errors.New("set AWS_REGION or configure a region in the AWS profile")
	}
	// 큐에서 메시지를 받기 전에 Swift 인증과 대상 컨테이너 접근을 확인한다.
	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	destination, err := openSwift(startupCtx)
	if err != nil {
		return err
	}
	err = destination.check(startupCtx)
	cancel()
	if err != nil {
		return err
	}
	worker := queueWorker{
		queue:    sqs.NewFromConfig(cfg),
		queueURL: queueURL,
		refresh:  visibilityRefresh,
		input: s3Input{
			client:      s3.NewFromConfig(cfg),
			bucket:      bucket,
			destination: destination,
		},
	}
	healthAddr := os.Getenv("COLLECTOR_HEALTH_ADDR")
	if healthAddr == "" {
		healthAddr = ":8080"
	}
	return worker.serve(ctx, healthAddr, destination.check)
}

func (w queueWorker) poll(ctx context.Context, queueReady *atomic.Bool) {
	defer queueReady.Store(false)
	for ctx.Err() == nil {
		pollCtx, cancel := context.WithTimeout(ctx, pollCallTimeout)
		result, err := w.queue.ReceiveMessage(pollCtx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(w.queueURL),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     pollWaitSeconds,
			VisibilityTimeout:   visibilitySeconds,
		})
		cancel()
		// 메시지가 없는 응답도 수신 요청이 성공했으므로 준비 상태로 본다.
		queueReady.Store(err == nil && ctx.Err() == nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("receive SQS message: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, message := range result.Messages {
			if err := w.handle(ctx, message); err != nil {
				log.Printf("SQS message %q was not acknowledged: %v", aws.ToString(message.MessageId), err)
			} else {
				log.Printf("deleted SQS message %q", aws.ToString(message.MessageId))
			}
		}
	}
}

func (w queueWorker) handle(ctx context.Context, message types.Message) error {
	if aws.ToString(message.ReceiptHandle) == "" {
		return errors.New("missing SQS receipt handle")
	}
	messageCtx, cancelMessage := context.WithCancelCause(ctx)
	defer cancelMessage(nil)
	// 객체를 읽고 저장하는 동안 별도 고루틴에서 메시지 숨김 시간을 연장한다.
	refreshCtx, stopRefresh := context.WithCancel(messageCtx)
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		w.refreshVisibility(refreshCtx, message.ReceiptHandle, cancelMessage)
	}()

	err := w.input.process(messageCtx, aws.ToString(message.Body))
	// 삭제와 숨김 시간 연장 요청이 겹치지 않도록 연장 작업이 끝날 때까지 기다린다.
	stopRefresh()
	<-refreshDone
	if cause := context.Cause(messageCtx); cause != nil {
		return cause
	}
	if err != nil {
		return err
	}

	// 알림에 포함된 객체를 모두 Swift에 저장한 뒤에만 SQS에서 삭제한다.
	deleteCtx, cancel := context.WithTimeout(ctx, queueCallTimeout)
	defer cancel()
	_, err = w.queue.DeleteMessage(deleteCtx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(w.queueURL),
		ReceiptHandle: message.ReceiptHandle,
	})
	if err != nil {
		return fmt.Errorf("delete SQS message: %w", err)
	}
	return nil
}

func (w queueWorker) refreshVisibility(ctx context.Context, receiptHandle *string, abort context.CancelCauseFunc) {
	ticker := time.NewTicker(w.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, queueCallTimeout)
			_, err := w.queue.ChangeMessageVisibility(callCtx, &sqs.ChangeMessageVisibilityInput{
				QueueUrl:          aws.String(w.queueURL),
				ReceiptHandle:     receiptHandle,
				VisibilityTimeout: visibilitySeconds,
			})
			cancel()
			if err != nil && ctx.Err() == nil {
				// 연장에 실패하면 현재 처리를 취소해 SQS에서 재전달할 수 있게 한다.
				abort(fmt.Errorf("extend SQS visibility: %w", err))
				return
			}
		}
	}
}
