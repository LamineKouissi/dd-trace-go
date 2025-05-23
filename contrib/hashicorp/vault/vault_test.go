// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package vault

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/mocktracer"
	"github.com/DataDog/dd-trace-go/v2/instrumentation"

	"github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/assert"
)

const secretMountPath = "/ns1/ns2/secret"

func setupServer(t *testing.T) (*httptest.Server, func()) {
	storage := make(map[string]string)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			slurp, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			storage[r.URL.Path] = string(slurp)
			fmt.Fprintln(w, "{}")
		case http.MethodGet:
			val, ok := storage[r.URL.Path]
			if !ok {
				http.Error(w, "No data for key.", http.StatusNotFound)
				return
			}
			secret := api.Secret{Data: make(map[string]interface{})}
			json.Unmarshal([]byte(val), &secret.Data)
			if err := json.NewEncoder(w).Encode(secret); err != nil {
				t.Fatal(err)
			}
		}
	}))
	return ts, func() {
		ts.Close()
	}
}

func setupClient(ts *httptest.Server) (*api.Client, error) {
	config := &api.Config{
		HttpClient: NewHTTPClient(),
		Address:    ts.URL,
	}
	client, err := api.NewClient(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func TestNewHTTPClient(t *testing.T) {
	ts, cleanup := setupServer(t)
	defer cleanup()

	client, err := setupClient(ts)
	if err != nil {
		t.Fatal(err)
	}
	testMountReadWrite(client, t)
}

func TestWrapHTTPClient(t *testing.T) {
	ts, cleanup := setupServer(t)
	defer cleanup()

	httpClient := http.Client{}
	config := &api.Config{
		HttpClient: WrapHTTPClient(&httpClient),
		Address:    ts.URL,
	}
	client, err := api.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	client.SetToken("myroot")

	testMountReadWrite(client, t)
}

// mountKV mounts the K/V engine on secretMountPath and returns a function to unmount it.
// See: https://www.vaultproject.io/docs/secrets/
func mountKV(c *api.Client, t *testing.T) func() {
	secretMount := api.MountInput{
		Type:        "kv",
		Description: "Test KV Store",
		Local:       true,
	}
	if err := c.Sys().Mount(secretMountPath, &secretMount); err != nil {
		t.Fatal(err)
	}
	return func() {
		c.Sys().Unmount(secretMountPath)
	}
}

func testMountReadWrite(c *api.Client, t *testing.T) {
	key := secretMountPath + "/test"
	fullPath := "/v1" + key
	data := map[string]interface{}{"Key1": "Val1", "Key2": "Val2"}

	hostname := ""
	url, err := url.Parse(c.Address())
	if err == nil {
		hostname = strings.TrimPrefix(url.Hostname(), "www.")
	}

	t.Run("mount", func(t *testing.T) {
		assert := assert.New(t)
		mt := mocktracer.Start()
		defer mt.Stop()
		defer mountKV(c, t)()

		spans := mt.FinishedSpans()
		assert.Len(spans, 1)
		span := spans[0]

		// Mount operation
		assert.Equal("vault", span.Tag(ext.ServiceName))
		assert.Equal("http.request", span.OperationName())
		assert.Equal("/v1/sys/mounts/ns1/ns2/secret", span.Tag(ext.HTTPURL))
		assert.Equal(http.MethodPost, span.Tag(ext.HTTPMethod))
		assert.Equal(http.MethodPost+" /v1/sys/mounts/ns1/ns2/secret", span.Tag(ext.ResourceName))
		assert.Equal(ext.SpanTypeHTTP, span.Tag(ext.SpanType))
		assert.Equal(float64(200), span.Tag(ext.HTTPCode))
		assert.Zero(span.Tag(ext.ErrorMsg))
		assert.Nil(span.Tag(ext.ErrorMsg))
		assert.Nil(span.Tag("vault.namespace"))
		assert.Equal("hashicorp/vault", span.Tag(ext.Component))
		assert.Equal(string(instrumentation.PackageHashicorpVaultAPI), span.Integration())
		assert.Equal(ext.SpanKindClient, span.Tag(ext.SpanKind))
		assert.Equal(hostname, span.Tag(ext.NetworkDestinationName))
	})

	t.Run("write", func(t *testing.T) {
		assert := assert.New(t)
		mt := mocktracer.Start()
		defer mt.Stop()
		defer mountKV(c, t)()

		// Write key
		_, err := c.Logical().Write(key, data)
		if err != nil {
			t.Fatal(err)
		}
		spans := mt.FinishedSpans()
		assert.Len(spans, 2)
		span := spans[1]

		assert.Equal("vault", span.Tag(ext.ServiceName))
		assert.Equal("http.request", span.OperationName())
		assert.Equal(fullPath, span.Tag(ext.HTTPURL))
		assert.Equal(http.MethodPut, span.Tag(ext.HTTPMethod))
		assert.Equal(http.MethodPut+" "+fullPath, span.Tag(ext.ResourceName))
		assert.Equal(ext.SpanTypeHTTP, span.Tag(ext.SpanType))
		assert.Equal(float64(200), span.Tag(ext.HTTPCode))
		assert.Zero(span.Tag(ext.ErrorMsg))
		assert.Nil(span.Tag(ext.ErrorMsg))
		assert.Nil(span.Tag("vault.namespace"))
		assert.Equal("hashicorp/vault", span.Tag(ext.Component))
		assert.Equal(string(instrumentation.PackageHashicorpVaultAPI), span.Integration())
		assert.Equal(ext.SpanKindClient, span.Tag(ext.SpanKind))
		assert.Equal(hostname, span.Tag(ext.NetworkDestinationName))
	})

	t.Run("read", func(t *testing.T) {
		assert := assert.New(t)
		mt := mocktracer.Start()
		defer mt.Stop()
		defer mountKV(c, t)()

		// Write the key first
		_, err := c.Logical().Write(key, data)
		if err != nil {
			t.Fatal(err)
		}
		// Read key
		secret, err := c.Logical().Read(key)
		if err != nil {
			t.Fatal(err)
		}
		spans := mt.FinishedSpans()
		assert.Len(spans, 3)
		span := spans[2]

		assert.Equal(secret.Data["Key1"], data["Key1"])
		assert.Equal(secret.Data["Key2"], data["Key2"])
		assert.Equal("vault", span.Tag(ext.ServiceName))
		assert.Equal("http.request", span.OperationName())
		assert.Equal(fullPath, span.Tag(ext.HTTPURL))
		assert.Equal(http.MethodGet, span.Tag(ext.HTTPMethod))
		assert.Equal(http.MethodGet+" "+fullPath, span.Tag(ext.ResourceName))
		assert.Equal(ext.SpanTypeHTTP, span.Tag(ext.SpanType))
		assert.Equal(float64(200), span.Tag(ext.HTTPCode))
		assert.Zero(span.Tag(ext.ErrorMsg))
		assert.Nil(span.Tag(ext.ErrorMsg))
		assert.Nil(span.Tag("vault.namespace"))
		assert.Equal("hashicorp/vault", span.Tag(ext.Component))
		assert.Equal(string(instrumentation.PackageHashicorpVaultAPI), span.Integration())
		assert.Equal(ext.SpanKindClient, span.Tag(ext.SpanKind))
		assert.Equal(hostname, span.Tag(ext.NetworkDestinationName))
	})
}

func TestReadError(t *testing.T) {
	assert := assert.New(t)
	mt := mocktracer.Start()
	defer mt.Stop()

	ts, cleanup := setupServer(t)
	defer cleanup()
	client, err := setupClient(ts)
	if err != nil {
		t.Fatal(err)
	}
	defer mountKV(client, t)()

	hostname := ""
	url, err := url.Parse(client.Address())
	if err == nil {
		hostname = strings.TrimPrefix(url.Hostname(), "www.")
	}

	key := "/some/bad/key"
	fullPath := "/v1" + key
	secret, err := client.Logical().Read(key)
	if err == nil {
		t.Fatalf("Expected error when reading key from %s, but it returned: %#v", key, secret)
	}
	spans := mt.FinishedSpans()
	assert.Len(spans, 2)
	span := spans[1]

	// Read key error
	assert.Equal("vault", span.Tag(ext.ServiceName))
	assert.Equal("http.request", span.OperationName())
	assert.Equal(fullPath, span.Tag(ext.HTTPURL))
	assert.Equal(http.MethodGet, span.Tag(ext.HTTPMethod))
	assert.Equal(http.MethodGet+" "+fullPath, span.Tag(ext.ResourceName))
	assert.Equal(ext.SpanTypeHTTP, span.Tag(ext.SpanType))
	assert.Equal(float64(404), span.Tag(ext.HTTPCode))
	assert.Equal("404: Not Found", span.Tag(ext.ErrorMsg))
	assert.NotNil(span.Tag(ext.ErrorMsg))
	assert.Nil(span.Tag("vault.namespace"))
	assert.Equal("hashicorp/vault", span.Tag(ext.Component))
	assert.Equal(string(instrumentation.PackageHashicorpVaultAPI), span.Integration())
	assert.Equal(ext.SpanKindClient, span.Tag(ext.SpanKind))
	assert.Equal(hostname, span.Tag(ext.NetworkDestinationName))
}

func TestNamespace(t *testing.T) {
	ts, cleanup := setupServer(t)
	defer cleanup()
	client, err := setupClient(ts)
	if err != nil {
		t.Fatal(err)
	}
	defer mountKV(client, t)()

	namespace := "/some/namespace"
	client.SetNamespace(namespace)
	key := secretMountPath + "/testNamespace"
	fullPath := "/v1" + key

	hostname := ""
	url, err := url.Parse(client.Address())
	if err == nil {
		hostname = strings.TrimPrefix(url.Hostname(), "www.")
	}

	t.Run("write", func(t *testing.T) {
		assert := assert.New(t)
		mt := mocktracer.Start()
		defer mt.Stop()

		// Write key with namespace
		data := map[string]interface{}{"Key1": "Val1", "Key2": "Val2"}
		_, err = client.Logical().Write(key, data)
		if err != nil {
			t.Fatal(err)
		}
		spans := mt.FinishedSpans()
		assert.Len(spans, 1)
		span := spans[0]

		assert.Equal("vault", span.Tag(ext.ServiceName))
		assert.Equal("http.request", span.OperationName())
		assert.Equal("http.request", span.OperationName())
		assert.Equal(fullPath, span.Tag(ext.HTTPURL))
		assert.Equal(http.MethodPut, span.Tag(ext.HTTPMethod))
		assert.Equal(http.MethodPut+" "+fullPath, span.Tag(ext.ResourceName))
		assert.Equal(ext.SpanTypeHTTP, span.Tag(ext.SpanType))
		assert.Equal(float64(200), span.Tag(ext.HTTPCode))
		assert.Zero(span.Tag(ext.ErrorMsg))
		assert.Nil(span.Tag(ext.ErrorMsg))
		assert.Equal(namespace, span.Tag("vault.namespace"))
		assert.Equal("hashicorp/vault", span.Tag(ext.Component))
		assert.Equal(string(instrumentation.PackageHashicorpVaultAPI), span.Integration())
		assert.Equal(ext.SpanKindClient, span.Tag(ext.SpanKind))
		assert.Equal(hostname, span.Tag(ext.NetworkDestinationName))
	})

	t.Run("read", func(t *testing.T) {
		assert := assert.New(t)
		mt := mocktracer.Start()
		defer mt.Stop()

		// Write key with namespace first
		data := map[string]interface{}{"Key1": "Val1", "Key2": "Val2"}
		_, err = client.Logical().Write(key, data)
		if err != nil {
			t.Fatal(err)
		}
		// Read key with namespace
		_, err = client.Logical().Read(key)
		if err != nil {
			t.Fatal(err)
		}
		spans := mt.FinishedSpans()
		assert.Len(spans, 2)
		span := spans[1]

		assert.Equal("vault", span.Tag(ext.ServiceName))
		assert.Equal("http.request", span.OperationName())
		assert.Equal(fullPath, span.Tag(ext.HTTPURL))
		assert.Equal(http.MethodGet, span.Tag(ext.HTTPMethod))
		assert.Equal(http.MethodGet+" "+fullPath, span.Tag(ext.ResourceName))
		assert.Equal(ext.SpanTypeHTTP, span.Tag(ext.SpanType))
		assert.Equal(float64(200), span.Tag(ext.HTTPCode))
		assert.Zero(span.Tag(ext.ErrorMsg))
		assert.Nil(span.Tag(ext.ErrorMsg))
		assert.Equal(namespace, span.Tag("vault.namespace"))
		assert.Equal("hashicorp/vault", span.Tag(ext.Component))
		assert.Equal(string(instrumentation.PackageHashicorpVaultAPI), span.Integration())
		assert.Equal(ext.SpanKindClient, span.Tag(ext.SpanKind))
		assert.Equal(hostname, span.Tag(ext.NetworkDestinationName))
	})
}

func TestOption(t *testing.T) {
	ts, cleanup := setupServer(t)
	defer cleanup()

	for ttName, tt := range map[string]struct {
		opts []Option
		test func(assert *assert.Assertions, span *mocktracer.Span)
	}{
		"DefaultOptions": {
			opts: []Option{},
			test: func(assert *assert.Assertions, span *mocktracer.Span) {
				assert.Equal(defaultServiceName, span.Tag(ext.ServiceName))
				assert.Nil(span.Tag(ext.EventSampleRate))
			},
		},
		"CustomServiceName": {
			opts: []Option{WithService("someServiceName")},
			test: func(assert *assert.Assertions, span *mocktracer.Span) {
				assert.Equal("someServiceName", span.Tag(ext.ServiceName))
			},
		},
		"WithAnalyticsTrue": {
			opts: []Option{WithAnalytics(true)},
			test: func(assert *assert.Assertions, span *mocktracer.Span) {
				assert.Equal(1.0, span.Tag(ext.EventSampleRate))
			},
		},
		"WithAnalyticsFalse": {
			opts: []Option{WithAnalytics(false)},
			test: func(assert *assert.Assertions, span *mocktracer.Span) {
				assert.Nil(span.Tag(ext.EventSampleRate))
			},
		},
		"WithAnalyticsLastOptionWins": {
			opts: []Option{WithAnalyticsRate(0.7), WithAnalytics(true)},
			test: func(assert *assert.Assertions, span *mocktracer.Span) {
				assert.Equal(1.0, span.Tag(ext.EventSampleRate))
			},
		},
		"WithAnalyticsRateMax": {
			opts: []Option{WithAnalyticsRate(1.0)},
			test: func(assert *assert.Assertions, span *mocktracer.Span) {
				assert.Equal(1.0, span.Tag(ext.EventSampleRate))
			},
		},
		"WithAnalyticsRateMin": {
			opts: []Option{WithAnalyticsRate(0.0)},
			test: func(assert *assert.Assertions, span *mocktracer.Span) {
				assert.Equal(0.0, span.Tag(ext.EventSampleRate))
			},
		},
		"WithAnalyticsRateLastOptionWins": {
			opts: []Option{WithAnalytics(true), WithAnalyticsRate(0.7)},
			test: func(assert *assert.Assertions, span *mocktracer.Span) {
				assert.Equal(0.7, span.Tag(ext.EventSampleRate))
			},
		},
	} {
		t.Run(ttName, func(t *testing.T) {
			assert := assert.New(t)
			config := &api.Config{
				HttpClient: NewHTTPClient(tt.opts...),
				Address:    ts.URL,
			}
			client, err := api.NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			defer mountKV(client, t)()

			mt := mocktracer.Start()
			defer mt.Stop()

			_, err = client.Logical().Write(
				secretMountPath+"/key",
				map[string]interface{}{"Key1": "Val1", "Key2": "Val2"},
			)
			if err != nil {
				t.Fatal(err)
			}
			spans := mt.FinishedSpans()
			assert.Len(spans, 1)
			span := spans[0]
			tt.test(assert, span)
		})
	}
}
