package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type objectClient interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type objectDestination interface {
	put(ctx context.Context, key, sourceID string, body io.Reader, size int64) error
}

type s3Input struct {
	client      objectClient
	bucket      string
	destination objectDestination
}

// s3Notification은 SQS 본문에 들어오는 S3 알림 형식이다. 알림에는 객체 위치가 담긴다.
type s3Notification struct {
	Service string `json:"Service"`
	Event   string `json:"Event"`
	Bucket  string `json:"Bucket"`
	Records []struct {
		Version string `json:"eventVersion"`
		Source  string `json:"eventSource"`
		Name    string `json:"eventName"`
		S3      struct {
			Bucket struct {
				Name string `json:"name"`
			} `json:"bucket"`
			Object struct {
				Key       string `json:"key"`
				VersionID string `json:"versionId"`
				ETag      string `json:"eTag"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

func (input s3Input) process(ctx context.Context, body string) error {
	var notification s3Notification
	if err := json.Unmarshal([]byte(body), &notification); err != nil {
		return fmt.Errorf("decode S3 notification: %w", err)
	}
	// 알림 설정 시 S3가 보내는 테스트 이벤트에는 읽을 객체가 없다.
	if notification.Event == "s3:TestEvent" && notification.Service == "Amazon S3" && notification.Bucket == input.bucket && len(notification.Records) == 0 {
		return nil
	}
	if len(notification.Records) == 0 {
		return errors.New("S3 notification has no object records")
	}
	for index, event := range notification.Records {
		major, minor, _ := strings.Cut(event.Version, ".")
		minorVersion, err := strconv.Atoi(minor)
		if major != "2" || err != nil || minorVersion < 1 || event.Source != "aws:s3" || !strings.HasPrefix(event.Name, "ObjectCreated:") {
			return fmt.Errorf("S3 notification record %d is not a supported object creation event", index+1)
		}
		object := event.S3.Object
		// S3 알림의 키는 URL 인코딩되며 공백은 +로 전달된다.
		key, err := url.QueryUnescape(object.Key)
		if err != nil || event.S3.Bucket.Name != input.bucket || !strings.HasPrefix(key, "firehose/processed/") || !strings.HasSuffix(key, ".jsonl.gz") {
			return fmt.Errorf("S3 notification record %d is outside the configured Firehose input", index+1)
		}
		request := &s3.GetObjectInput{Bucket: aws.String(input.bucket), Key: aws.String(key)}
		// 알림이 가리키는 버전을 읽는다. 버전이 없고 ETag가 있으면 내용 변경을 검사한다.
		if object.VersionID != "" {
			request.VersionId = aws.String(object.VersionID)
		} else if object.ETag != "" {
			request.IfMatch = aws.String(strconv.Quote(object.ETag))
		}
		if err := input.copyObject(ctx, request); err != nil {
			return fmt.Errorf("S3 notification record %d: %w", index+1, err)
		}
	}
	return nil
}

func (input s3Input) copyObject(ctx context.Context, request *s3.GetObjectInput) error {
	object, err := input.client.GetObject(ctx, request)
	if err != nil {
		return fmt.Errorf("get S3 object: %w", err)
	}
	defer object.Body.Close()
	stream := bufio.NewReader(object.Body)
	// 압축 표식만 확인하고 CronJob에 전달할 GZIP 바이트는 그대로 유지한다.
	header, err := stream.Peek(3)
	if err != nil || !bytes.Equal(header, []byte{0x1f, 0x8b, 0x08}) {
		return errors.New("S3 object is not gzip")
	}
	size := aws.ToInt64(object.ContentLength)
	if size < 3 {
		return errors.New("S3 object is missing a valid content length")
	}
	// 같은 키가 덮어써졌을 때 이전 객체의 재전달과 구분한다.
	version := aws.ToString(object.VersionId)
	if version == "" {
		version = aws.ToString(request.VersionId)
	}
	etag := strings.Trim(aws.ToString(object.ETag), `"`)
	if version == "" && etag == "" {
		return errors.New("S3 object is missing both version and ETag")
	}
	key := aws.ToString(request.Key)
	source := strings.Join([]string{input.bucket, key, version, etag}, "\x00")
	sourceID := fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
	return input.destination.put(ctx, key, sourceID, stream, size)
}
