package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

func TestSwiftPutVerifiesReplayAndPreservesCompressedBytes(t *testing.T) {
	const key = "firehose/processed/year=2026/month=11/day=11/example.jsonl.gz"
	const sourceID = "source-a"
	content := gzipBody(t, "{\"ts\":0}\n")
	etag := fmt.Sprintf("%x", md5.Sum(content))
	for _, test := range []struct {
		name           string
		status         int
		existingSource string
		refreshToken   bool
		wantCalls      []string
		wantError      string
	}{
		{"new object", 201, "", false, []string{"PUT"}, ""},
		{"token renewal", 201, "", true, []string{"PUT", "PUT"}, ""},
		{"same object replay", 412, sourceID, false, []string{"PUT", "HEAD"}, ""},
		{"different object", 412, "source-b", false, []string{"PUT", "HEAD"}, "different source or content"},
		{"Swift unavailable", 503, "", false, []string{"PUT"}, "write Swift object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			provider := &gophercloud.ProviderClient{}
			provider.SetToken("old-token")
			if test.refreshToken {
				provider.ReauthFunc = func(context.Context) error {
					provider.SetToken("renewed-token")
					return nil
				}
			}
			provider.HTTPClient.Transport = swiftRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls = append(calls, request.Method)
				if request.URL.Path != "/v1/AUTH_test/telemetry/"+key {
					t.Fatalf("unexpected Swift path %q", request.URL.Path)
				}
				response := &http.Response{StatusCode: test.status, Header: make(http.Header), Body: http.NoBody}
				switch request.Method {
				case http.MethodPut:
					defer request.Body.Close()
					body, err := io.ReadAll(request.Body)
					if err != nil || !bytes.Equal(body, content) {
						t.Fatalf("Swift received different gzip bytes: %v", err)
					}
					if request.Header.Get("If-None-Match") != "*" || request.Header.Get("ETag") != etag ||
						request.Header.Get("X-Object-Meta-Source-Id") != sourceID || request.Header.Get("Content-Type") != "application/gzip" ||
						request.Header.Get("Content-Encoding") != "" {
						t.Fatalf("unexpected object headers: %v", request.Header)
					}
					if test.refreshToken && request.Header.Get("X-Auth-Token") == "old-token" {
						response.StatusCode = http.StatusUnauthorized
					}
				case http.MethodHead:
					response.StatusCode = http.StatusOK
					response.Header.Set("X-Object-Meta-Source-Id", test.existingSource)
					response.Header.Set("Content-Length", fmt.Sprint(len(content)))
					response.Header.Set("ETag", etag)
				default:
					t.Fatalf("unexpected method %s", request.Method)
				}
				return response, nil
			})
			store := swiftStore{client: &gophercloud.ServiceClient{
				ProviderClient: provider, Endpoint: "https://swift.example/v1/AUTH_test/",
			}, container: "telemetry"}
			err := store.put(context.Background(), key, sourceID, bytes.NewReader(content), int64(len(content)))
			if test.wantError == "" && err != nil || test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("put error = %v, want %q", err, test.wantError)
			}
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatalf("HTTP methods = %v, want %v", calls, test.wantCalls)
			}
		})
	}
}

type swiftRoundTrip func(*http.Request) (*http.Response, error)

func (f swiftRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
