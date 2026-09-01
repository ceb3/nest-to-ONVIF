package cli

import (
	"log/slog"
	"testing"
)

func TestLogLevelFromEnv(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"nonsense", slog.LevelInfo},
	} {
		t.Setenv("NEST_BRIDGE_LOG_LEVEL", tc.in)
		if got := logLevelFromEnv(); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.in, got, tc.want)
		}
	}
}
