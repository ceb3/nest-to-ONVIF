package setup

import (
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunSystemDiscoveryIncludesPlatformChecks(t *testing.T) {
	d := runSystemDiscovery(".", "deploy")
	require.NotEmpty(t, d.Checks)
	assert.Equal(t, runtime.GOOS, d.OS)

	var linux, macvlan Requirement
	for _, c := range d.Checks {
		switch c.ID {
		case "linux":
			linux = c
		case "macvlan":
			macvlan = c
		}
	}
	require.NotEmpty(t, linux.ID)
	require.NotEmpty(t, macvlan.ID)
	if runtime.GOOS == "linux" {
		assert.Equal(t, RequirementPass, linux.Status)
		assert.True(t, linux.Required)
	} else {
		assert.Equal(t, RequirementFail, linux.Status)
		assert.False(t, d.Ready)
	}
}

func TestRequireReadyBlocksWhenNotReady(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("requires non-linux to test failure path")
	}
	s := &Server{repoRoot: ".", deployDir: "deploy"}
	rec := &fakeResponseWriter{}
	assert.False(t, s.requireReady(rec))
	assert.Equal(t, 503, rec.status)
}

type fakeResponseWriter struct {
	status int
	body   []byte
	header http.Header
}

func (f *fakeResponseWriter) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}
func (f *fakeResponseWriter) Write(b []byte) (int, error) {
	f.body = append(f.body, b...)
	return len(b), nil
}
func (f *fakeResponseWriter) WriteHeader(statusCode int) { f.status = statusCode }
