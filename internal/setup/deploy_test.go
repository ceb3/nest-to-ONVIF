package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatchComposeHostIPPreservesLoopback(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(compose, []byte(`
services:
  mediamtx:
    ports:
      - "203.0.113.1:8554:8554"
      - "127.0.0.1:8554:8554"
      - "203.0.113.1:8888:8888"
      - "127.0.0.1:8888:8888"
  snapshots:
    ports:
      - "203.0.113.1:8080:80"
      - "127.0.0.1:8080:80"
`), 0o644))

	require.NoError(t, patchComposeHostIP(dir, "192.168.1.20", &bytes.Buffer{}))
	raw, err := os.ReadFile(compose)
	require.NoError(t, err)
	text := string(raw)
	assert.Contains(t, text, `"192.168.1.20:8554:8554"`)
	assert.Contains(t, text, `"127.0.0.1:8554:8554"`)
	assert.Contains(t, text, `"192.168.1.20:8888:8888"`)
	assert.Contains(t, text, `"127.0.0.1:8888:8888"`)
	assert.Contains(t, text, `"192.168.1.20:8080:80"`)
	assert.Contains(t, text, `"127.0.0.1:8080:80"`)
	assert.NotContains(t, text, `"203.0.113.1:`)
}

func TestPatchComposeHostIPNoOpWhenAlreadySet(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "docker-compose.yml")
	body := `ports:
      - "192.168.1.15:8554:8554"
      - "127.0.0.1:8554:8554"
`
	require.NoError(t, os.WriteFile(compose, []byte(body), 0o644))
	require.NoError(t, patchComposeHostIP(dir, "192.168.1.15", &bytes.Buffer{}))
	raw, err := os.ReadFile(compose)
	require.NoError(t, err)
	assert.Equal(t, body, string(raw))
}
