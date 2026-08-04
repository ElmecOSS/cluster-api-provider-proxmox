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
	"net/http"
	"slices"
	"strings"
	"sync"

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
}

// New returns a caching Factory. A single instance is meant to be shared by
// all reconcilers of the controller manager.
func New() Factory {
	return &cachingFactory{clients: map[types.NamespacedName]entry{}}
}

type entry struct {
	// dataHash fingerprints the credential-relevant secret data. Cache
	// validity is keyed on data, not resourceVersion: the cluster controller
	// patches finalizers and ownerRefs onto these secrets, which bumps the
	// resourceVersion without changing credentials.
	dataHash [sha256.Size]byte
	client   capmox.Client
}

type cachingFactory struct {
	mu      sync.Mutex
	clients map[types.NamespacedName]entry
}

func (f *cachingFactory) GetOrCreate(ctx context.Context, logger logr.Logger, secret *corev1.Secret) (capmox.Client, error) {
	key := types.NamespacedName{Namespace: secret.GetNamespace(), Name: secret.GetName()}
	hash := hashSecretData(secret)

	f.mu.Lock()
	defer f.mu.Unlock()

	if e, ok := f.clients[key]; ok && e.dataHash == hash {
		return e.client, nil
	}

	// Construction errors are not cached; the next reconcile retries.
	c, err := NewClientFromSecret(ctx, logger, secret)
	if err != nil {
		return nil, err
	}

	f.clients[key] = entry{dataHash: hash, client: c}
	return c, nil
}

func hashSecretData(secret *corev1.Secret) [sha256.Size]byte {
	h := sha256.New()
	for _, k := range []string{"url", "token", "secret", "insecure", "root_ca"} {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(secret.Data[k])
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
	token := string(secret.Data["token"])
	tokenSecret := string(secret.Data["secret"])
	url := string(secret.Data["url"])

	tlsInsecure, tlsInsecureSet := secret.Data["insecure"]
	tlsRootCA := secret.Data["root_ca"]

	rootCerts, err := tlshelper.SystemRootsWithCert(tlsRootCA)
	if err != nil {
		return nil, fmt.Errorf("loading cert pool: %w", err)
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
	}

	httpClient := &http.Client{Transport: tr}
	return goproxmox.NewAPIClient(ctx, logger, url,
		proxmox.WithHTTPClient(httpClient),
		proxmox.WithAPIToken(token, tokenSecret),
	)
}
