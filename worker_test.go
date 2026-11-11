package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const objectEvent = `{"eventVersion":"2.1","eventSource":"aws:s3","eventName":"ObjectCreated:Put",
	"s3":{"bucket":{"name":"test-bucket"},"object":{"key":"firehose%2Fprocessed%2Ftest+file.jsonl.gz","versionId":"v1"}}}`

func TestWorkerDeletesOnlyAfterAllObjectsAreProcessed(t *testing.T) {
	// 모의 클라이언트로 객체 읽기, 저장, 메시지 삭제의 순서를 확인한다.
	for _, test := range []struct {
		name        string
		body        string
		failOnWrite int
		wantActions []string
	}{
		{"multiple objects", `{"Records":[` + objectEvent + `,` + objectEvent + `]}`, 0, []string{"get", "commit", "get", "commit", "delete"}},
		{"partial failure", `{"Records":[` + objectEvent + `,` + objectEvent + `]}`, 2, []string{"get", "commit", "get", "failed write"}},
		{"test event", `{"Service":"Amazon S3","Event":"s3:TestEvent","Bucket":"test-bucket"}`, 0, []string{"delete"}},
		{"unexpected bucket", `{"Records":[` + strings.ReplaceAll(objectEvent, "test-bucket", "other-bucket") + `]}`, 0, nil},
		{"legacy message", `{"idempotencyKey":"demo-001","vehicleId":"vehicle-001"}`, 0, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			const records = "{\"idempotencyKey\":\"test\"}\n"
			var actions []string
			writes := 0
			worker := queueWorker{
				queue:   &testQueue{delete: func() { actions = append(actions, "delete") }},
				refresh: time.Hour,
				input: s3Input{
					bucket: "test-bucket",
					client: testObject(func(_ context.Context, request *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
						actions = append(actions, "get")
						if aws.ToString(request.Key) != "firehose/processed/test file.jsonl.gz" || aws.ToString(request.VersionId) != "v1" {
							t.Fatalf("unexpected S3 request: %+v", request)
						}
						return &s3.GetObjectOutput{Body: gzipBody(t, records)}, nil
					}),
					ingest: func(_ context.Context, stream io.Reader) error {
						content, err := io.ReadAll(stream)
						if err != nil || string(content) != records {
							t.Fatalf("unexpected gzip content: %q, %v", content, err)
						}
						writes++
						if writes == test.failOnWrite {
							actions = append(actions, "failed write")
							return errors.New("test database error")
						}
						actions = append(actions, "commit")
						return nil
					},
				},
			}
			err := worker.handle(context.Background(), types.Message{Body: aws.String(test.body), ReceiptHandle: aws.String("receipt")})
			wantError := test.wantActions == nil || test.failOnWrite > 0
			if (err != nil) != wantError || !reflect.DeepEqual(actions, test.wantActions) {
				t.Fatalf("got actions %v and error %v, want %v and error=%t", actions, err, test.wantActions, wantError)
			}
		})
	}
}

func TestWorkerCancelsProcessingWhenVisibilityCannotBeExtended(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	visibilityError := errors.New("test visibility error")
	worker := queueWorker{
		queue: &testQueue{
			delete: func() { t.Error("message was deleted after visibility extension failed") },
			extend: func(request *sqs.ChangeMessageVisibilityInput) error {
				if request.VisibilityTimeout != 120 || aws.ToString(request.ReceiptHandle) != "receipt" {
					t.Errorf("unexpected visibility request: %+v", request)
				}
				return visibilityError
			},
		},
		refresh: time.Millisecond,
		input: s3Input{
			bucket: "test-bucket",
			client: testObject(func(ctx context.Context, _ *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
				// 읽기가 진행 중일 때 연장 실패가 취소 신호로 전달되는지 확인한다.
				<-ctx.Done()
				return nil, ctx.Err()
			}),
		},
	}
	err := worker.handle(ctx, types.Message{Body: aws.String(`{"Records":[` + objectEvent + `]}`), ReceiptHandle: aws.String("receipt")})
	if !errors.Is(err, visibilityError) {
		t.Fatalf("want visibility error, got %v", err)
	}
}

type testQueue struct {
	delete func()
	extend func(*sqs.ChangeMessageVisibilityInput) error
}

func (*testQueue) ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	panic("unexpected receive")
}

func (q *testQueue) ChangeMessageVisibility(_ context.Context, request *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	return nil, q.extend(request)
}

func (q *testQueue) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	q.delete()
	return &sqs.DeleteMessageOutput{}, nil
}

type testObject func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error)

func (f testObject) GetObject(ctx context.Context, request *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return f(ctx, request)
}

func gzipBody(t *testing.T, content string) io.ReadCloser {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(writer, content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return io.NopCloser(&compressed)
}
