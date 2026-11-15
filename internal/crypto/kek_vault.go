package crypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Vault Transit as the key-encrypting key.
//
//	hcvault://vault.internal:8200/transit/keys/toft
//
// The host, the mount and the key name come out of the URI; nothing else is
// configured. Transit never returns the key — it encrypts and decrypts on the
// server, which is exactly the property that makes it a KEK.
//
// **Spoken over plain HTTP rather than through the Vault SDK.** Two calls are
// needed, `transit/encrypt` and `transit/decrypt`, and a third for Kubernetes
// login; the SDK is a very large dependency for that, and this product's
// dependency budget is not a style preference — every package here is one an
// air-gapped customer's security team reads.
//
// Authentication comes from the ambient chain and never from the URI:
// `VAULT_TOKEN` when it is set, otherwise Kubernetes auth with the pod's own
// service-account token. The second is what an in-cluster install actually has,
// and it renews itself, which is why a token that expires is not an outage.

const (
	vaultTokenHeader = "X-Vault-Token" //nolint:gosec // a header name, not a credential
	// vaultK8sTokenPath is where every Kubernetes pod finds its own projected
	// service-account token.
	vaultK8sTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // a path
)

type vaultTransit struct {
	uri     string
	addr    string // https://host:port
	mount   string // transit
	keyName string
	role    string // the Kubernetes auth role, when that is how we log in
	client  *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newVaultTransit(uri string) (KEK, error) {
	rest := strings.TrimPrefix(uri, SchemeVault)
	host, path, ok := strings.Cut(rest, "/")
	if !ok || host == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY_URI: %s needs host/mount/keys/name, e.g. "+
			"%svault.internal:8200/transit/keys/toft", SchemeVault, SchemeVault)
	}
	// mount/keys/name — the shape Vault's own paths use.
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[1] != "keys" || parts[0] == "" || parts[2] == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY_URI: %q should be <mount>/keys/<name>, e.g. transit/keys/toft", path)
	}

	// https unless the operator wrote a scheme into the host, which is how a
	// development Vault on http is reached without a second variable.
	addr := "https://" + host
	if s, h, ok := strings.Cut(host, "+"); ok && (s == "http" || s == "https") {
		addr = s + "://" + h
	}

	return &vaultTransit{
		uri:     uri,
		addr:    addr,
		mount:   parts[0],
		keyName: parts[2],
		role:    envOr("VAULT_K8S_ROLE", "toft"),
		client:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func (v *vaultTransit) URI() string { return v.uri }

func (v *vaultTransit) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	err := v.call(ctx, "/v1/"+v.mount+"/encrypt/"+v.keyName,
		map[string]string{"plaintext": base64.StdEncoding.EncodeToString(dek)}, &out)
	if err != nil {
		return nil, err
	}
	if out.Data.Ciphertext == "" {
		return nil, fmt.Errorf("vault returned no ciphertext")
	}
	// Transit's ciphertext is "vault:v1:…" — an opaque string, stored verbatim,
	// which is what carries the key version through a Vault-side rotation.
	return []byte(out.Data.Ciphertext), nil
}

func (v *vaultTransit) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	err := v.call(ctx, "/v1/"+v.mount+"/decrypt/"+v.keyName,
		map[string]string{"ciphertext": string(wrapped)}, &out)
	if err != nil {
		return nil, err
	}
	dek, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault returned a plaintext that is not base64: %w", err)
	}
	return dek, nil
}

func (v *vaultTransit) call(ctx context.Context, path string, body any, out any) error {
	token, err := v.authToken(ctx)
	if err != nil {
		return err
	}
	return v.post(ctx, path, token, body, out)
}

func (v *vaultTransit) post(ctx context.Context, path, token string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.addr+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(vaultTokenHeader, token)
	}

	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Bounded: a misconfigured address can be anything at all, and an error
	// message is not a place to put a megabyte of it.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vault %s: %s: %s", path, resp.Status,
			strings.TrimSpace(string(payload[:min(len(payload), 400)])))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// authToken returns a usable token, logging in through Kubernetes auth when
// VAULT_TOKEN is absent. Cached until shortly before it expires, so a long-
// running platform renews rather than failing.
func (v *vaultTransit) authToken(ctx context.Context) (string, error) {
	if t := strings.TrimSpace(os.Getenv("VAULT_TOKEN")); t != "" {
		return t, nil
	}

	v.mu.Lock()
	if v.token != "" && time.Now().Before(v.expires) {
		t := v.token
		v.mu.Unlock()
		return t, nil
	}
	v.mu.Unlock()

	jwt, err := os.ReadFile(vaultK8sTokenPath)
	if err != nil {
		return "", fmt.Errorf("no VAULT_TOKEN and no Kubernetes service-account token to log in with: %w", err)
	}

	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := v.post(ctx, "/v1/auth/kubernetes/login", "", map[string]string{
		"role": v.role,
		"jwt":  strings.TrimSpace(string(jwt)),
	}, &out); err != nil {
		return "", fmt.Errorf("vault kubernetes login as role %q: %w", v.role, err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("vault kubernetes login returned no token")
	}

	lease := time.Duration(out.Auth.LeaseDuration) * time.Second
	if lease <= 0 {
		lease = time.Hour
	}

	v.mu.Lock()
	v.token = out.Auth.ClientToken
	// Renew at four fifths of the lease: a token that expires between the
	// check and the call is an outage nobody can act on.
	v.expires = time.Now().Add(lease * 4 / 5)
	v.mu.Unlock()
	return out.Auth.ClientToken, nil
}
