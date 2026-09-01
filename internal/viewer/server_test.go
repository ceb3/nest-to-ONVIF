package viewer_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/viewer"
)

func TestHandleCameras(t *testing.T) {
	cfg := &config.Config{
		Cameras: []config.Camera{
			{Name: "Front Door", Audio: true, ONVIF: config.ONVIFConfig{IP: "192.168.1.8"}},
			{Name: "Driveway", EventsEnabled: true, ONVIF: config.ONVIFConfig{IP: "192.168.1.9"}},
		},
	}
	srv := viewer.NewServer(cfg, true, viewer.NewEventBus())

	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	req.Host = "192.168.1.15:8090"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Cameras  []viewer.CameraView `json:"cameras"`
		EventsOn bool                `json:"events_on"`
		PageSize int                 `json:"page_size"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Len(t, body.Cameras, 2)
	assert.True(t, body.EventsOn)
	assert.Equal(t, 6, body.PageSize)
	assert.Equal(t, "cam-front-door", body.Cameras[0].Path)
	assert.Contains(t, body.Cameras[0].HLSLQ, "http://192.168.1.15:8888/cam-front-door-lq/index.m3u8")
	assert.True(t, body.Cameras[0].Audio)
	assert.True(t, body.Cameras[1].Events)
}
