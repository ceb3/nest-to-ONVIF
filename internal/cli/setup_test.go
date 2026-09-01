package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSetupListenRejectsNonLoopback(t *testing.T) {
	require.Error(t, validateSetupListen("192.168.1.15:8190"))
	require.Error(t, validateSetupListen("0.0.0.0:8190"))
	require.NoError(t, validateSetupListen("127.0.0.1:8190"))
	require.NoError(t, validateSetupListen("localhost:8190"))
}

func TestValidateSetupListenRejectsInvalid(t *testing.T) {
	assert.Error(t, validateSetupListen("not-an-address"))
}
