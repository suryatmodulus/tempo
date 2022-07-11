package gcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	raw "google.golang.org/api/storage/v1"
)

func TestHedge(t *testing.T) {
	tests := []struct {
		name                   string
		returnIn               time.Duration
		hedgeAt                time.Duration
		expectedHedgedRequests int32
	}{
		{
			name:                   "hedge disabled",
			expectedHedgedRequests: 1,
		},
		{
			name:                   "hedge enabled doesn't hit",
			hedgeAt:                time.Hour,
			expectedHedgedRequests: 1,
		},
		{
			name:                   "hedge enabled and hits",
			hedgeAt:                time.Millisecond,
			returnIn:               100 * time.Millisecond,
			expectedHedgedRequests: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			count := int32(0)
			server := fakeServer(t, tc.returnIn, &count)

			r, w, _, err := New(&Config{
				BucketName:        "blerg",
				Insecure:          true,
				Endpoint:          server.URL,
				HedgeRequestsAt:   tc.hedgeAt,
				HedgeRequestsUpTo: 2,
			})
			require.NoError(t, err)

			ctx := context.Background()

			// the first call on each client initiates an extra http request
			// clearing that here
			_, _, _ = r.Read(ctx, "object", []string{"test"}, false)
			time.Sleep(tc.returnIn)
			atomic.StoreInt32(&count, 0)

			// calls that should hedge
			_, _, _ = r.Read(ctx, "object", []string{"test"}, false)
			time.Sleep(tc.returnIn)
			assert.Equal(t, tc.expectedHedgedRequests, atomic.LoadInt32(&count))
			atomic.StoreInt32(&count, 0)

			_ = r.ReadRange(ctx, "object", []string{"test"}, 10, []byte{}, false)
			time.Sleep(tc.returnIn)
			assert.Equal(t, tc.expectedHedgedRequests, atomic.LoadInt32(&count))
			atomic.StoreInt32(&count, 0)

			// calls that should not hedge
			_, _ = r.List(ctx, []string{"test"})
			assert.Equal(t, int32(1), atomic.LoadInt32(&count))
			atomic.StoreInt32(&count, 0)

			_ = w.Write(ctx, "object", []string{"test"}, bytes.NewReader([]byte{}), 0, false)
			assert.Equal(t, int32(1), atomic.LoadInt32(&count))
			atomic.StoreInt32(&count, 0)
		})
	}
}

func TestReadError(t *testing.T) {
	errA := storage.ErrObjectNotExist
	errB := readError(errA)
	assert.Equal(t, backend.ErrDoesNotExist, errB)

	wups := fmt.Errorf("wups")
	errB = readError(wups)
	assert.Equal(t, wups, errB)
}

func TestObjectConfigAttributes(t *testing.T) {
	tests := []struct {
		name           string
		cacheControl   string
		metadata       map[string]string
		expectedObject raw.Object
	}{
		{
			name:           "cache controle enabled",
			cacheControl:   "no-cache",
			expectedObject: raw.Object{Name: "test/object", Bucket: "blerg2", CacheControl: "no-cache"},
		},
		{
			name:           "medata set",
			metadata:       map[string]string{"one": "1"},
			expectedObject: raw.Object{Name: "test/object", Bucket: "blerg2", Metadata: map[string]string{"one": "1"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rawObject := raw.Object{}
			server := fakeServerWithObjectAttributes(t, &rawObject)

			_, w, _, err := New(&Config{
				BucketName:         "blerg2",
				Endpoint:           server.URL,
				Insecure:           true,
				ObjectCacheControl: tc.cacheControl,
				ObjectMetadata:     tc.metadata,
			})
			require.NoError(t, err)

			ctx := context.Background()

			_ = w.Write(ctx, "object", []string{"test"}, bytes.NewReader([]byte{}), 0, false)
			assert.Equal(t, tc.expectedObject, rawObject)
		})
	}
}

func fakeServer(t *testing.T, returnIn time.Duration, counter *int32) *httptest.Server {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(returnIn)

		atomic.AddInt32(counter, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	server.StartTLS()
	t.Cleanup(server.Close)

	return server
}

func fakeServerWithObjectAttributes(t *testing.T, o *raw.Object) *httptest.Server {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		// Check that we are making the call to update the attributes before attempting to decode the request body.
		if strings.HasPrefix(r.RequestURI, "/upload/storage/v1/b/blerg2") {

			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			require.NoError(t, err)

			reader := multipart.NewReader(r.Body, params["boundary"])
			defer r.Body.Close()

			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				defer part.Close()

				switch part.Header.Get("Content-Type") {
				case "application/json":
					err = json.NewDecoder(r.Body).Decode(&o)
					require.NoError(t, err)
				}
			}
		}

		_, _ = w.Write([]byte(`{}`))
	}))
	server.StartTLS()
	t.Cleanup(server.Close)

	return server
}
