package main

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/objectstorage/v1/containers"
	"github.com/gophercloud/gophercloud/v2/openstack/objectstorage/v1/objects"
)

type swiftStore struct {
	client    *gophercloud.ServiceClient
	container string
}

func openSwift(ctx context.Context) (swiftStore, error) {
	container := os.Getenv("COLLECTOR_SWIFT_CONTAINER")
	if container == "" {
		return swiftStore{}, errors.New("COLLECTOR_SWIFT_CONTAINER is required")
	}
	credentials := gophercloud.AuthOptions{
		IdentityEndpoint:            os.Getenv("OS_AUTH_URL"),
		ApplicationCredentialID:     os.Getenv("OS_APPLICATION_CREDENTIAL_ID"),
		ApplicationCredentialSecret: os.Getenv("OS_APPLICATION_CREDENTIAL_SECRET"),
		AllowReauth:                 true,
	}
	if credentials.IdentityEndpoint == "" || credentials.ApplicationCredentialID == "" || credentials.ApplicationCredentialSecret == "" {
		return swiftStore{}, errors.New("OS_AUTH_URL, OS_APPLICATION_CREDENTIAL_ID and OS_APPLICATION_CREDENTIAL_SECRET are required")
	}
	provider, err := openstack.AuthenticatedClient(ctx, credentials)
	if err != nil {
		return swiftStore{}, fmt.Errorf("authenticate to OpenStack: %w", err)
	}
	client, err := openstack.NewObjectStorageV1(provider, gophercloud.EndpointOpts{
		Region:       os.Getenv("OS_REGION_NAME"),
		Availability: gophercloud.Availability(os.Getenv("OS_INTERFACE")),
	})
	if err != nil {
		return swiftStore{}, fmt.Errorf("find Swift endpoint: %w", err)
	}
	return swiftStore{client: client, container: container}, nil
}

func (s swiftStore) check(ctx context.Context) error {
	if err := containers.Get(ctx, s.client, s.container, nil).Err; err != nil {
		return fmt.Errorf("check Swift container: %w", err)
	}
	return nil
}

func (s swiftStore) put(ctx context.Context, key, sourceID string, body io.Reader, size int64) error {
	// 임시 파일을 쓰면 전체 객체를 메모리에 올리지 않고 토큰 갱신 시에도 다시 전송할 수 있다.
	file, err := os.CreateTemp("", "collector-*.jsonl.gz")
	if err != nil {
		return fmt.Errorf("create temporary object file: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	digest := md5.New()
	written, err := io.Copy(io.MultiWriter(file, digest), body)
	if err != nil {
		return fmt.Errorf("read S3 object into temporary file: %w", err)
	}
	if written != size {
		return fmt.Errorf("S3 object length mismatch: expected %d bytes, got %d", size, written)
	}

	// Swift는 일반 객체의 MD5를 ETag로 받아 전송된 바이트를 검증한다.
	etag := fmt.Sprintf("%x", digest.Sum(nil))
	err = objects.Create(ctx, s.client, s.container, key, objects.CreateOpts{
		Content:     io.NewSectionReader(file, 0, size),
		ContentType: "application/gzip",
		ETag:        etag,
		IfNoneMatch: "*",
		Metadata:    map[string]string{"Source-Id": sourceID},
	}).Err
	if err == nil {
		return nil
	}
	if !gophercloud.ResponseCodeIs(err, http.StatusPreconditionFailed) {
		return fmt.Errorf("write Swift object: %w", err)
	}

	// 재전달로 이미 저장된 객체인지 확인한다. 같은 이름에 다른 원본이 있으면 보존한다.
	existing := objects.Get(ctx, s.client, s.container, key, objects.GetOpts{Newest: true})
	if existing.Err != nil {
		return fmt.Errorf("verify existing Swift object: %w", existing.Err)
	}
	if existing.Header.Get("X-Object-Meta-Source-Id") != sourceID || existing.Header.Get("Content-Length") != strconv.FormatInt(size, 10) ||
		strings.Trim(existing.Header.Get("ETag"), `"`) != etag {
		return errors.New("Swift object already exists with different source or content")
	}
	return nil
}
