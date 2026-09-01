package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCLIArgsHonorsDurationBeforeStreamCommand(t *testing.T) {
	args, err := parseCLIArgs([]string{"-for=37m", "stream", "Front Door"})

	require.NoError(t, err)
	assert.Equal(t, "stream", args.command)
	assert.Equal(t, "Front Door", args.cameraName)
	assert.Equal(t, 37*time.Minute, args.streamFor)
}

func TestParseCLIArgsRejectsDurationAfterCameraName(t *testing.T) {
	_, err := parseCLIArgs([]string{"stream", "Front Door", "-for=37m"})

	require.EqualError(t, err,
		`flags must precede the command; use: nest-bridge [-for=DURATION] stream <camera-name>`)
}

func TestParseCLIArgsReportsUsageWhenCameraNameIsMissing(t *testing.T) {
	for _, args := range [][]string{
		{"stream"},
		{"-for=5m", "stream"},
		{"stream", ""},
	} {
		_, err := parseCLIArgs(args)

		require.EqualError(t, err,
			`usage: nest-bridge [-for=DURATION] stream <camera-name>`,
			"args: %q", args)
	}
}

func TestParseCLIArgsDefaultsStreamDurationToOneHour(t *testing.T) {
	args, err := parseCLIArgs([]string{"stream", "Front Door"})

	require.NoError(t, err)
	assert.Equal(t, time.Hour, args.streamFor)
}
