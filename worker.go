package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const (
	visibilitySeconds = 120
	visibilityRefresh = 60 * time.Second
	pollWaitSeconds   = 20
	pollCallTimeout   = 30 * time.Second
	queueCallTimeout  = 10 * time.Second
)

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

func runWorker(ctx context.Context, db *sql.DB) error {
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
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to MySQL: %w", err)
	}
	worker := queueWorker{
		queue:    sqs.NewFromConfig(cfg),
		queueURL: queueURL,
		refresh:  visibilityRefresh,
		input: s3Input{
			client: s3.NewFromConfig(cfg),
			bucket: bucket,
			ingest: func(ctx context.Context, input io.Reader) error {
				return ingest(ctx, db, input)
			},
		},
	}
	log.Print("collector worker started")
	worker.poll(ctx)
	return nil
}

func (w queueWorker) poll(ctx context.Context) {
	for ctx.Err() == nil {
		pollCtx, cancel := context.WithTimeout(ctx, pollCallTimeout)
		result, err := w.queue.ReceiveMessage(pollCtx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(w.queueURL),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     pollWaitSeconds,
			VisibilityTimeout:   visibilitySeconds,
		})
		cancel()
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
	refreshCtx, stopRefresh := context.WithCancel(messageCtx)
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		w.refreshVisibility(refreshCtx, message.ReceiptHandle, cancelMessage)
	}()

	err := w.input.process(messageCtx, aws.ToString(message.Body))
	stopRefresh()
	<-refreshDone
	if cause := context.Cause(messageCtx); cause != nil {
		return cause
	}
	if err != nil {
		return err
	}

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
				abort(fmt.Errorf("extend SQS visibility: %w", err))
				return
			}
		}
	}
}
