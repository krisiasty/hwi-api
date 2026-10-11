package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunIgnoresInertMTLSWithoutTLS: with TLS off, an enabled mTLS block
// is inert, so the agent must start without reading its CA file, which
// here does not exist, and still authenticate with the token.
func TestRunIgnoresInertMTLSWithoutTLS(t *testing.T) {
	const token = "test-token"
	sum := sha256.Sum256([]byte(token))
	dir := t.TempDir()
	cfg := `
api:
  bind: { addresses: ["127.0.0.1"], port: 18449 }
  tls: { enabled: false }
  auth:
    mtls: { enabled: true, client_ca_file: /nonexistent/client-ca.pem }
    token: { enabled: true, hashes: ["sha256:` + hex.EncodeToString(sum[:]) + `"] }
  access: { allow: ["127.0.0.1/32"] }
ops:
  bind: { addresses: ["127.0.0.1"], port: 19106 }
log: { level: error }
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	errCh, cancel := startRun(t, []string{"--config", path}, io.Discard)

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-errCh:
			t.Fatalf("run exited during startup: %v", err)
		default:
		}
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:18449/api/v1/inventory", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("token-authenticated request = %d, want 200", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the API listener never came up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}
