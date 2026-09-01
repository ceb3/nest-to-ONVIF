package session

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/stdnet"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validOffer = "v=0\r\n" +
	"o=- 1 1 IN IP4 0.0.0.0\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0 1 2\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"a=mid:0\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=recvonly\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
	"a=mid:1\r\n" +
	"a=recvonly\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"a=mid:2\r\n" +
	"a=sendrecv\r\n" +
	"a=sctp-port:5000\r\n"

func TestOfferMeetsGoogleRequirements(t *testing.T) {
	pc, err := NewPeerConnection(true)
	require.NoError(t, err)
	defer func() { _ = pc.Close() }()

	offer, err := CreateOffer(context.Background(), pc)
	require.NoError(t, err)

	audio := strings.Index(offer, "m=audio")
	video := strings.Index(offer, "m=video")
	app := strings.Index(offer, "m=application")
	require.NotEqual(t, -1, audio, "offer must contain m=audio")
	require.NotEqual(t, -1, video, "offer must contain m=video")
	require.NotEqual(t, -1, app, "offer must contain m=application (the data channel)")
	require.Less(t, audio, video, "audio must precede video")
	require.Less(t, video, app, "video must precede application")

	audioSection := offer[audio:video]
	videoSection := offer[video:app]
	applicationSection := offer[app:]
	assert.Contains(t, audioSection, "a=recvonly", "audio must be receive-only")
	assert.Contains(t, videoSection, "a=recvonly", "video must be receive-only")
	assert.Contains(t, strings.ToLower(audioSection), "opus", "audio must offer Opus")
	assert.NotContains(t, audioSection, "a=sendrecv", "audio must not send media")
	assert.NotContains(t, videoSection, "a=sendrecv", "video must not send media")
	assert.Contains(t, applicationSection, "a=sendrecv", "the data channel is bidirectional")
	assert.True(t, strings.HasSuffix(offer, "\n"), "offer must end with a newline")

	var parsed sdp.SessionDescription
	require.NoError(t, parsed.UnmarshalString(offer))
	require.Len(t, parsed.MediaDescriptions, 3)

	realMIDs := make(map[string]struct{}, 3)
	for _, section := range parsed.MediaDescriptions {
		require.NotNil(t, section)
		mid, ok := section.Attribute("mid")
		require.True(t, ok, "%s section must have a MID", section.MediaName.Media)
		require.NotEmpty(t, mid, "%s section MID must not be empty", section.MediaName.Media)
		realMIDs[mid] = struct{}{}
	}
	require.Len(t, realMIDs, 3, "each required section must have a distinct MID")

	var bundleMIDs []string
	for _, attribute := range parsed.Attributes {
		fields := strings.Fields(attribute.Value)
		if attribute.Key == "group" && len(fields) > 0 && fields[0] == "BUNDLE" {
			require.Empty(t, bundleMIDs, "offer must have exactly one BUNDLE group")
			bundleMIDs = fields[1:]
		}
	}
	require.Len(t, bundleMIDs, 3, "BUNDLE must reference all required sections")
	bundled := make(map[string]struct{}, 3)
	for _, mid := range bundleMIDs {
		_, ok := realMIDs[mid]
		assert.True(t, ok, "BUNDLE must reference a real section MID")
		bundled[mid] = struct{}{}
	}
	assert.Len(t, bundled, 3, "BUNDLE must reference each required section MID")
}

func TestValidateOfferRejectsMisorderedMLines(t *testing.T) {
	bad := strings.Replace(validOffer,
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:0\r\na=rtpmap:111 opus/48000/2\r\na=recvonly\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\na=recvonly\r\n",
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\na=recvonly\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:0\r\na=rtpmap:111 opus/48000/2\r\na=recvonly\r\n",
		1,
	)
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "order")
}

func TestValidateOfferRejectsMissingDataChannel(t *testing.T) {
	bad := strings.Replace(validOffer,
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:2\r\na=sendrecv\r\na=sctp-port:5000\r\n",
		"",
		1,
	)
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "m=application")
}

func TestValidateOfferRejectsMissingTrailingNewline(t *testing.T) {
	bad := strings.TrimSuffix(validOffer, "\n")
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newline")
}

func TestValidateOfferRejectsNonUnifiedPlan(t *testing.T) {
	bad := strings.Replace(validOffer, "a=group:BUNDLE 0 1 2\r\n", "", 1)
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unified Plan")
}

func TestValidateOfferRejectsAudioWithoutOpus(t *testing.T) {
	bad := strings.Replace(validOffer, "a=rtpmap:111 opus/48000/2\r\n", "", 1)
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Opus")
}

func TestValidateOfferRejectsNonRecvonlyAudio(t *testing.T) {
	bad := strings.Replace(validOffer, "a=recvonly\r\n", "a=sendrecv\r\n", 1)
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recvonly")
}

func TestValidateOfferRejectsRecvonlyLookalike(t *testing.T) {
	bad := strings.Replace(validOffer, "a=recvonly\r\n", "a=recvonly-extra\r\n", 1)
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recvonly")
}

func TestValidateOfferRejectsNonRecvonlyVideo(t *testing.T) {
	bad := strings.Replace(validOffer,
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\na=recvonly\r\n",
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\na=sendrecv\r\n",
		1,
	)
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recvonly")
}

func TestValidateOfferAllowsSendrecvDataChannel(t *testing.T) {
	require.NoError(t, validateOffer(validOffer))
}

func TestValidateOfferAllowsArbitraryConsistentMIDs(t *testing.T) {
	offer := strings.NewReplacer(
		"a=group:BUNDLE 0 1 2", "a=group:BUNDLE data-main audio-main video-main",
		"a=mid:0", "a=mid:audio-main",
		"a=mid:1", "a=mid:video-main",
		"a=mid:2", "a=mid:data-main",
	).Replace(validOffer)

	require.NoError(t, validateOffer(offer))
}

func TestValidateOfferRejectsDuplicateMIDs(t *testing.T) {
	bad := strings.NewReplacer(
		"a=group:BUNDLE 0 1 2", "a=group:BUNDLE shared shared data",
		"a=mid:0", "a=mid:shared",
		"a=mid:1", "a=mid:shared",
		"a=mid:2", "a=mid:data",
	).Replace(validOffer)

	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unified Plan")
}

func TestValidateOfferRejectsOpusMappedToUnusedPayloadType(t *testing.T) {
	bad := strings.Replace(validOffer, "a=rtpmap:111 opus/48000/2", "a=rtpmap:112 opus/48000/2", 1)
	err := validateOffer(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Opus")
}

func TestValidateOfferRejectsInvalidDataChannel(t *testing.T) {
	tests := map[string]string{
		"wrong protocol": strings.Replace(
			validOffer,
			"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
			"m=application 9 UDP/TLS/RTP/SAVPF webrtc-datachannel",
			1,
		),
		"wrong format": strings.Replace(
			validOffer,
			"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
			"m=application 9 UDP/DTLS/SCTP bogus",
			1,
		),
		"missing SCTP port": strings.Replace(validOffer, "a=sctp-port:5000\r\n", "", 1),
		"invalid SCTP port": strings.Replace(
			validOffer,
			"a=sctp-port:5000",
			"a=sctp-port:not-a-port",
			1,
		),
	}

	for name, offer := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateOffer(offer)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "data channel")
			assert.NotContains(t, err.Error(), "bogus")
			assert.NotContains(t, err.Error(), "not-a-port")
		})
	}
}

func TestValidateOfferRejectsMalformedOpusEncoding(t *testing.T) {
	tests := map[string]string{
		"non-numeric clock rate": "opus/garbage",
		"wrong clock rate":       "opus/8000/2",
		"missing channels":       "opus/48000",
		"wrong channels":         "opus/48000/1",
		"trailing field":         "opus/48000/2/extra",
	}

	for name, encoding := range tests {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(validOffer, "opus/48000/2", encoding, 1)
			err := validateOffer(bad)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "Opus")
			assert.NotContains(t, err.Error(), encoding)
		})
	}
}

func TestValidateOfferAllowsCaseInsensitiveOpusName(t *testing.T) {
	offer := strings.Replace(validOffer, "opus/48000/2", "OPUS/48000/2", 1)
	require.NoError(t, validateOffer(offer))
}

func TestValidateOfferRejectsDuplicateOrUnexpectedMediaSection(t *testing.T) {
	tests := map[string]string{
		"duplicate audio": strings.Replace(
			validOffer,
			"m=video 9 UDP/TLS/RTP/SAVPF 96",
			"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:duplicate\r\na=recvonly\r\nm=video 9 UDP/TLS/RTP/SAVPF 96",
			1,
		),
		"unexpected text": validOffer +
			"m=text 9 UDP/TLS/RTP/SAVPF 98\r\n" +
			"a=mid:unexpected\r\n" +
			"a=recvonly\r\n",
	}

	for name, offer := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateOffer(offer)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "media sections")
		})
	}
}

func TestValidateOfferRejectsMalformedInputWithoutPanic(t *testing.T) {
	tests := map[string]string{
		"empty":        "",
		"truncated":    "v=0\r\n",
		"binary noise": "\x00\xff\x01\xfe",
		"incomplete m": "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\nm=audio\r\n",
	}

	for name, offer := range tests {
		t.Run(name, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() {
				err = validateOffer(offer)
			})
			require.Error(t, err)
		})
	}
}

func TestCreateOfferHonorsCallerCancellationDuringGathering(t *testing.T) {
	pc, readStarted, releaseReads := newPeerConnectionWithBlockedNetworkRead(t)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := CreateOffer(ctx, pc)
		result <- err
	}()

	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("ICE gathering did not start a network read")
	}
	require.Equal(t, webrtc.ICEGatheringStateGathering, pc.ICEGatheringState())
	cancel()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("CreateOffer did not return promptly after cancellation")
	}
	releaseReads()
	require.Eventually(t, func() bool {
		return pc.ConnectionState() == webrtc.PeerConnectionStateClosed
	}, time.Second, 10*time.Millisecond, "peer connection did not finish closing")
	require.NoError(t, pc.GracefulClose())
}

func TestCreateOfferHonorsShorterCallerDeadline(t *testing.T) {
	pc, _, releaseReads := newPeerConnectionWithBlockedNetworkRead(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := CreateOffer(ctx, pc)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
	releaseReads()
	require.Eventually(t, func() bool {
		return pc.ConnectionState() == webrtc.PeerConnectionStateClosed
	}, time.Second, 10*time.Millisecond, "peer connection did not finish closing")
	require.NoError(t, pc.GracefulClose())
}

func TestClosePeerConnectionAsyncReturnsWhileGracefulCloseIsBlocked(t *testing.T) {
	closer := newBlockingGracefulCloser()
	closePeerConnectionAsync(closer)

	select {
	case <-closer.gracefulStarted:
	case <-time.After(time.Second):
		t.Fatal("graceful close did not start")
	}

	select {
	case <-closer.forceClosed:
		t.Fatal("cleanup attempted a second close while graceful close was blocked")
	case <-time.After(200 * time.Millisecond):
	}

	close(closer.releaseGraceful)
	select {
	case <-closer.gracefulFinished:
	case <-time.After(time.Second):
		t.Fatal("graceful close did not finish after it was released")
	}
}

func TestClosePeerConnectionAsyncDoesNotForceCloseAfterPromptGracefulClose(t *testing.T) {
	closer := newPromptGracefulCloser()

	closePeerConnectionAsync(closer)

	select {
	case <-closer.gracefulFinished:
	case <-time.After(time.Second):
		t.Fatal("graceful close did not finish")
	}
	select {
	case <-closer.forceClosed:
		t.Fatal("prompt graceful close triggered forceful close")
	case <-time.After(200 * time.Millisecond):
	}
}

type blockingGracefulCloser struct {
	gracefulStarted  chan struct{}
	gracefulFinished chan struct{}
	forceClosed      chan struct{}
	releaseGraceful  chan struct{}
	startOnce        sync.Once
	closeOnce        sync.Once
}

func newBlockingGracefulCloser() *blockingGracefulCloser {
	return &blockingGracefulCloser{
		gracefulStarted:  make(chan struct{}),
		gracefulFinished: make(chan struct{}),
		forceClosed:      make(chan struct{}),
		releaseGraceful:  make(chan struct{}),
	}
}

func (c *blockingGracefulCloser) GracefulClose() error {
	c.startOnce.Do(func() { close(c.gracefulStarted) })
	<-c.releaseGraceful
	close(c.gracefulFinished)
	return nil
}

func (c *blockingGracefulCloser) Close() error {
	c.closeOnce.Do(func() {
		close(c.forceClosed)
	})
	return nil
}

type promptGracefulCloser struct {
	gracefulFinished chan struct{}
	forceClosed      chan struct{}
}

func newPromptGracefulCloser() *promptGracefulCloser {
	return &promptGracefulCloser{
		gracefulFinished: make(chan struct{}),
		forceClosed:      make(chan struct{}),
	}
}

func (c *promptGracefulCloser) GracefulClose() error {
	close(c.gracefulFinished)
	return nil
}

func (c *promptGracefulCloser) Close() error {
	close(c.forceClosed)
	return nil
}

func newPeerConnectionWithBlockedNetworkRead(
	t *testing.T,
) (*webrtc.PeerConnection, <-chan struct{}, func()) {
	t.Helper()

	systemNet, err := stdnet.NewNet()
	require.NoError(t, err)
	readStarted := make(chan struct{})
	releaseReads := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseReads) })
	}

	blockedNet := &blockingReadNet{
		Net:          systemNet,
		readStarted:  readStarted,
		releaseReads: releaseReads,
	}
	settings := webrtc.SettingEngine{}
	settings.SetNet(blockedNet)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers:   []webrtc.ICEServer{{URLs: []string{"stun:192.0.2.1:3478"}}},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	t.Cleanup(release)

	_, err = pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	)
	require.NoError(t, err)
	_, err = pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	)
	require.NoError(t, err)
	_, err = pc.CreateDataChannel("dataSendChannel", nil)
	require.NoError(t, err)

	return pc, readStarted, release
}

type blockingReadNet struct {
	transport.Net
	readStarted  chan struct{}
	releaseReads <-chan struct{}
	readOnce     sync.Once
}

func (n *blockingReadNet) ListenUDP(network string, address *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.Net.ListenUDP(network, address)
	if err != nil {
		return nil, err
	}
	return &blockingReadUDPConn{
		UDPConn:      conn,
		readStarted:  n.readStarted,
		releaseReads: n.releaseReads,
		readOnce:     &n.readOnce,
	}, nil
}

type blockingReadUDPConn struct {
	transport.UDPConn
	readStarted  chan struct{}
	releaseReads <-chan struct{}
	readOnce     *sync.Once
}

func (c *blockingReadUDPConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	c.readOnce.Do(func() { close(c.readStarted) })
	<-c.releaseReads
	return c.UDPConn.ReadFrom(buffer)
}
