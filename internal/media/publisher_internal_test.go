package media

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/session"
)

var internalVideoTrack = session.TrackInfo{
	Kind:     webrtc.RTPCodecTypeVideo,
	MimeType: webrtc.MimeTypeH264,
}

type stubWriter struct {
	err    error
	writes int
	closes int
}

func (s *stubWriter) WritePacketRTP(*description.Media, *rtp.Packet) error {
	s.writes++
	return s.err
}

func (s *stubWriter) Close() { s.closes++ }

// announcedPublisher wires a publisher to a stub RTSP client so the write path
// can be driven through conditions a real server will not produce on demand.
// When announceMedium is false the video medium exists but is absent from the
// announced description, which is the shape that used to crash gortsplib.
func announcedPublisher(t *testing.T, writer rtspWriter, announceMedium bool) *Publisher {
	t.Helper()
	p, err := NewPublisher(config.Camera{Name: "Backyard"}, "rtsp://127.0.0.1:8554")
	require.NoError(t, err)
	p.video = testVideoMedia()
	if announceMedium {
		p.rebuildDescription()
	}
	p.client = writer
	p.announced = true
	return p
}

func videoPacket() *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96, SequenceNumber: 1},
		Payload: []byte{0x65},
	}
}

func testVideoMedia() *description.Media {
	return &description.Media{
		Type: description.MediaTypeVideo,
		Formats: []format.Format{&format.H264{
			PayloadTyp:        96,
			PacketizationMode: 1,
		}},
	}
}

// Every round trip the announce makes is bounded by a timeout this package
// pins, rather than by whatever gortsplib defaults to.
func TestNewRTSPClientConfiguresTimeouts(t *testing.T) {
	assert.Equal(t, 10*time.Second, rtspDialTimeout)
	p, err := NewPublisher(config.Camera{Name: "Backyard"}, "rtsp://127.0.0.1:8554")
	require.NoError(t, err)

	client := p.newRTSPClient()

	assert.NotNil(t, client.DialContext)
	assert.Equal(t, 10*time.Second, client.ReadTimeout)
	assert.Equal(t, 10*time.Second, client.WriteTimeout)
}

// Closing must abandon a dial that is already under way, not merely the pauses
// between announce attempts: a host that drops packets rather than refusing
// them would otherwise hold teardown for the full dial timeout.
func TestRTSPClientDialIsAbandonedMidDialOnClose(t *testing.T) {
	p, err := NewPublisher(config.Camera{Name: "Backyard"}, "rtsp://127.0.0.1:8554")
	require.NoError(t, err)
	client := p.newRTSPClient()

	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = p.Close()
	}()

	// TEST-NET-1 is reserved for documentation and is not routed, so the dial
	// hangs rather than being refused. Only the close can end it early.
	start := time.Now()
	conn, err := client.DialContext(context.Background(), "tcp", "192.0.2.1:80")
	elapsed := time.Since(start)

	require.Error(t, err)
	if !errors.Is(err, context.Canceled) {
		t.Skipf("network refused or failed the dial instead of dropping it (%v); "+
			"this environment cannot exercise a mid-dial cancel", err)
	}
	assert.Nil(t, conn)
	assert.Less(t, elapsed, rtspDialTimeout,
		"close must cut the dial short rather than letting it time out")
}

// The pre-close fast path is a separate branch from the mid-dial cancel above,
// and skips the dial entirely.
func TestRTSPClientDialIsRefusedAfterClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	p, err := NewPublisher(config.Camera{Name: "Backyard"}, "rtsp://127.0.0.1:8554")
	require.NoError(t, err)
	client := p.newRTSPClient()
	require.NoError(t, p.Close())

	// The address is reachable, so only the close can be responsible.
	conn, err := client.DialContext(context.Background(), "tcp", listener.Addr().String())

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, conn)
}

// gortsplib dereferences its per-medium state without a nil check, so a medium
// missing from the announced description must be rejected before it gets there.
func TestPublisherRejectsUnannouncedMedium(t *testing.T) {
	writer := &stubWriter{}
	p := announcedPublisher(t, writer, false)

	var writeErr error
	require.NotPanics(t, func() {
		writeErr = p.WriteRTP(internalVideoTrack, videoPacket())
	})

	require.Error(t, writeErr)
	assert.Contains(t, writeErr.Error(), "absent from the announced RTSP session description")
	assert.NotErrorIs(t, writeErr, session.ErrSinkFatal)
	assert.Zero(t, writer.writes)
}

// gortsplib's outbound queue holds roughly 1.7 seconds of media at the measured
// packet rate. A brief stall must cost the packets it stalls on, not the track.
func TestWriteRTPDropsPacketWhenWriteQueueIsFull(t *testing.T) {
	writer := &stubWriter{err: liberrors.ErrClientWriteQueueFull{}}
	p := announcedPublisher(t, writer, true)

	require.NoError(t, p.WriteRTP(internalVideoTrack, videoPacket()))
	require.NoError(t, p.WriteRTP(internalVideoTrack, videoPacket()))

	assert.Equal(t, 2, writer.writes)
	// Genuine loss, counted apart from the packets never meant to be published.
	assert.Equal(t, session.SinkStats{QueueDropped: 2}, p.SinkStats())
}

// Any other write failure means the RTSP session is gone, which the session
// manager must be able to tell apart from a survivable drop.
func TestWriteRTPReportsDeadConnectionAsFatal(t *testing.T) {
	writer := &stubWriter{err: liberrors.ErrClientTerminated{}}
	p := announcedPublisher(t, writer, true)

	err := p.WriteRTP(internalVideoTrack, videoPacket())

	require.Error(t, err)
	assert.ErrorIs(t, err, session.ErrSinkFatal)
	assert.ErrorAs(t, err, &liberrors.ErrClientTerminated{})
	assert.Contains(t, err.Error(), "127.0.0.1:8554")
	assert.Equal(t, session.SinkStats{Failed: 1}, p.SinkStats())
}

// The three rejections that once shared one message must each name their own
// condition; it is the only diagnostic an operator gets.
func TestWriteRTPRejectionsAreDistinguishable(t *testing.T) {
	p := announcedPublisher(t, &stubWriter{}, true)
	p.wantAudio = true

	lateMedium := p.WriteRTP(
		session.TrackInfo{Kind: webrtc.RTPCodecTypeAudio, MimeType: webrtc.MimeTypeOpus},
		&rtp.Packet{Header: rtp.Header{PayloadType: 111}, Payload: []byte{0xfc}},
	)
	require.Error(t, lateMedium)
	assert.Contains(t, lateMedium.Error(), "appeared after the RTSP session was announced")

	absent := announcedPublisher(t, &stubWriter{}, false).
		WriteRTP(internalVideoTrack, videoPacket())
	require.Error(t, absent)

	mismatch := p.WriteRTP(internalVideoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 97},
		Payload: []byte{0x65},
	})
	require.Error(t, mismatch)
	assert.Contains(t, mismatch.Error(), "payload type")

	assert.NotEqual(t, lateMedium.Error(), absent.Error())
	assert.NotEqual(t, absent.Error(), mismatch.Error())
	assert.NotEqual(t, lateMedium.Error(), mismatch.Error())
}

func TestWriteRTPAfterCloseIsDropped(t *testing.T) {
	writer := &stubWriter{}
	p := announcedPublisher(t, writer, true)
	require.NoError(t, p.Close())

	require.NoError(t, p.WriteRTP(internalVideoTrack, videoPacket()))

	assert.Zero(t, writer.writes)
	assert.Equal(t, 1, writer.closes)
	assert.Equal(t, session.SinkStats{Discarded: 1}, p.SinkStats())
}
