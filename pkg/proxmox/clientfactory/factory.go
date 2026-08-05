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

// Package clientfactory builds Proxmox API clients from credential Secrets
// and caches them across reconciles.
package clientfactory

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ionos-cloud/cluster-api-provider-proxmox/internal/tlshelper"
	capmox "github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/goproxmox"
)

// Factory builds and caches capmox.Client instances from credential Secrets.
type Factory interface {
	// GetOrCreate returns a client for the given credentials secret, reusing
	// a cached instance as long as the secret data is unchanged.
	GetOrCreate(ctx context.Context, logger logr.Logger, secret *corev1.Secret) (capmox.Client, error)

	// Evict drops the cached client for the given secret, if any. The next
	// GetOrCreate rebuilds it from scratch. Used when a cached client is
	// found to be unhealthy.
	Evict(secretNamespace, secretName string)
}

// New returns a caching Factory. A single instance is meant to be shared by
// all reconcilers of the controller manager.
func New() Factory {
	return &cachingFactory{clients: map[types.NamespacedName]*entry{}}
}

// entry serializes construction per secret so that a slow or unreachable
// endpoint never blocks lookups for other secrets: the factory-wide mutex is
// only ever held for map access, while the network dial happens under the
// per-entry mutex.
type entry struct {
	mu sync.Mutex

	// dataHash fingerprints the credential-relevant secret data. Cache
	// validity is keyed on data, not resourceVersion: the cluster controller
	// patches finalizers and ownerRefs onto these secrets, which bumps the
	// resourceVersion without changing credentials. Every key is written
	// with a presence marker so an absent key and a present-but-empty key
	// (different TLS semantics for "insecure") hash differently.
	dataHash [sha256.Size]byte
	client   capmox.Client
	// transport backs client and is kept to release idle connections when
	// the entry is replaced or evicted.
	transport *http.Transport
}

type cachingFactory struct {
	mu      sync.Mutex
	clients map[types.NamespacedName]*entry
}

func (f *cachingFactory) GetOrCreate(ctx context.Context, logger logr.Logger, secret *corev1.Secret) (capmox.Client, error) {
	key := types.NamespacedName{Namespace: secret.GetNamespace(), Name: secret.GetName()}
	hash := hashSecretData(secret)

	f.mu.Lock()
	e, ok := f.clients[key]
	if !ok {
		e = &entry{}
		f.clients[key] = e
	}
	f.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client != nil && e.dataHash == hash {
		return e.client, nil
	}

	// Construction errors are not cached; the next reconcile retries.
	c, tr, err := newClientFromSecret(ctx, logger, secret)
	if err != nil {
		return nil, err
	}

	if e.transport != nil {
		e.transport.CloseIdleConnections()
	}
	e.dataHash = hash
	e.client = c
	e.transport = tr
	return c, nil
}

func (f *cachingFactory) Evict(secretNamespace, secretName string) {
	key := types.NamespacedName{Namespace: secretNamespace, Name: secretName}

	f.mu.Lock()
	e, ok := f.clients[key]
	delete(f.clients, key)
	f.mu.Unlock()

	if !ok {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.transport != nil {
		e.transport.CloseIdleConnections()
	}
	e.client = nil
}

func hashSecretData(secret *corev1.Secret) [sha256.Size]byte {
	h := sha256.New()
	for _, k := range []string{"url", "token", "secret", "insecure", "root_ca"} {
		h.Write([]byte(k))
		h.Write([]byte{0})
		if v, present := secret.Data[k]; present {
			h.Write([]byte{1})
			h.Write(v)
		} else {
			h.Write([]byte{0})
		}
		h.Write([]byte{0})
	}

	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// NewClientFromSecret builds an uncached Proxmox API client from a
// credentials secret with the keys url, token, secret and the optional keys
// insecure and root_ca.
func NewClientFromSecret(ctx context.Context, logger logr.Logger, secret *corev1.Secret) (capmox.Client, error) {
	c, _, err := newClientFromSecret(ctx, logger, secret)
	return c, err
}

func newClientFromSecret(ctx context.Context, logger logr.Logger, secret *corev1.Secret) (capmox.Client, *http.Transport, error) {
	token := string(secret.Data["token"])
	tokenSecret := string(secret.Data["secret"])
	url := string(secret.Data["url"])

	tlsInsecure, tlsInsecureSet := secret.Data["insecure"]
	tlsRootCA := secret.Data["root_ca"]

	rootCerts, err := tlshelper.SystemRootsWithCert(tlsRootCA)
	if err != nil {
		return nil, nil, fmt.Errorf("loading cert pool: %w", err)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			// When "insecure" is unset we retain the pre-v0.7 behavior of
			// setting the connection insecure. If it is set we compare
			// against YAML true-ish values.
			//
			// #nosec:G402 // Intended to enable insecure mode for unknown CAs
			InsecureSkipVerify: !tlsInsecureSet || slices.Contains([]string{"1", "on", "true", "yes", "y"}, strings.ToLower(string(tlsInsecure))),
			RootCAs:            rootCerts,
		},
		// Bound connection establishment and first response so an
		// unreachable or blackholed endpoint cannot stall a reconcile for
		// the OS TCP timeout (or forever). Request bodies stay unbounded:
		// once headers arrive, long transfers are legitimate.
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}

	httpClient := &http.Client{Transport: tr}
	c, err := goproxmox.NewAPIClient(ctx, logger, url,
		proxmox.WithHTTPClient(httpClient),
		proxmox.WithAPIToken(token, tokenSecret),
	)
	if err != nil {
		return nil, nil, err
	}

	return c, tr, nil
}
