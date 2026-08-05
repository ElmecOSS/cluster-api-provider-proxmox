/*
Copyright 2023-2026 IONOS Cloud.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package clientfactory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newVersionServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var versionCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/version" {
			versionCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"release":"test"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	return srv, &versionCalls
}

func newCredentialsSecret(name, url string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: metav1.NamespaceDefault},
		Data: map[string][]byte{
			"url":    []byte(url),
			"token":  []byte("user@pve!token"),
			"secret": []byte("supersecret"),
		},
	}
}

func TestFactoryCachesClientPerSecretData(t *testing.T) {
	srv, versionCalls := newVersionServer(t)
	factory := New()
	secret := newCredentialsSecret("creds", srv.URL)

	first, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)

	second, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)

	require.Same(t, first, second)
	require.EqualValues(t, 1, versionCalls.Load())
}

func TestFactoryRebuildsClientOnDataChange(t *testing.T) {
	srv, versionCalls := newVersionServer(t)
	factory := New()
	secret := newCredentialsSecret("creds", srv.URL)

	first, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)

	// metadata-only updates (finalizers, ownerRefs) must not invalidate.
	secret.ResourceVersion = "9999"
	secret.Finalizers = []string{"capmox.example/finalizer"}
	cached, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)
	require.Same(t, first, cached)
	require.EqualValues(t, 1, versionCalls.Load())

	// credential rotation must rebuild.
	secret.Data["token"] = []byte("user@pve!rotated")
	rotated, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)
	require.NotSame(t, first, rotated)
	require.EqualValues(t, 2, versionCalls.Load())
}

func TestFactoryDoesNotCacheErrors(t *testing.T) {
	srv, versionCalls := newVersionServer(t)
	factory := New()

	// unreachable endpoint: construction fails, nothing is cached.
	broken := newCredentialsSecret("creds", "http://127.0.0.1:1")
	_, err := factory.GetOrCreate(context.Background(), logr.Discard(), broken)
	require.Error(t, err)

	// same secret name with a fixed url must succeed on retry.
	fixed := newCredentialsSecret("creds", srv.URL)
	c, err := factory.GetOrCreate(context.Background(), logr.Discard(), fixed)
	require.NoError(t, err)
	require.NotNil(t, c)
	require.EqualValues(t, 1, versionCalls.Load())
}

func TestFactoryDistinguishesAbsentFromEmptyKey(t *testing.T) {
	srv, versionCalls := newVersionServer(t)
	factory := New()
	secret := newCredentialsSecret("creds", srv.URL)

	first, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)

	// "insecure" absent and "insecure: \"\"" have different TLS semantics:
	// the cache must rebuild.
	secret.Data["insecure"] = []byte("")
	second, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)
	require.NotSame(t, first, second)
	require.EqualValues(t, 2, versionCalls.Load())
}

func TestFactoryEvict(t *testing.T) {
	srv, versionCalls := newVersionServer(t)
	factory := New()
	secret := newCredentialsSecret("creds", srv.URL)

	first, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)

	factory.Evict(secret.Namespace, secret.Name)

	second, err := factory.GetOrCreate(context.Background(), logr.Discard(), secret)
	require.NoError(t, err)
	require.NotSame(t, first, second)
	require.EqualValues(t, 2, versionCalls.Load())

	// evicting an unknown secret is a no-op.
	factory.Evict("nowhere", "nothing")
}

func TestFactorySlowEndpointDoesNotBlockOtherSecrets(t *testing.T) {
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"release":"slow"}}`))
	}))
	t.Cleanup(slowSrv.Close)
	fastSrv, _ := newVersionServer(t)

	factory := New()

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		_, _ = factory.GetOrCreate(context.Background(), logr.Discard(), newCredentialsSecret("slow-creds", slowSrv.URL))
		close(done)
	}()

	<-started
	time.Sleep(100 * time.Millisecond) // let the slow dial take its entry lock

	start := time.Now()
	_, err := factory.GetOrCreate(context.Background(), logr.Discard(), newCredentialsSecret("fast-creds", fastSrv.URL))
	require.NoError(t, err)
	require.Less(t, time.Since(start), time.Second, "a slow endpoint must not serialize other secrets")

	<-done
}

func TestFactoryKeysBySecretIdentity(t *testing.T) {
	srv, versionCalls := newVersionServer(t)
	factory := New()

	first, err := factory.GetOrCreate(context.Background(), logr.Discard(), newCredentialsSecret("zone-a", srv.URL))
	require.NoError(t, err)

	second, err := factory.GetOrCreate(context.Background(), logr.Discard(), newCredentialsSecret("zone-b", srv.URL))
	require.NoError(t, err)

	require.NotSame(t, first, second)
	require.EqualValues(t, 2, versionCalls.Load())
}
