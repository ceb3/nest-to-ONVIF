package media_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/media"
	"github.com/mustacheride/nest-to-ONVIF/internal/session"
)

var videoTrack = session.TrackInfo{
	Kind:     webrtc.RTPCodecTypeVideo,
	MimeType: webrtc.MimeTypeH264,
}

var audioTrack = session.TrackInfo{
	Kind:     webrtc.RTPCodecTypeAudio,
	MimeType: webrtc.MimeTypeOpus,
}

type capturedStream struct {
	mu            sync.Mutex
	desc          *description.Session
	pkts          []*rtp.Packet
	announces     int
	failAnnounces int
	announce      chan struct{}
	announceOnce  sync.Once
}

// failNextAnnounces makes the server reject the next n ANNOUNCE requests, so
// that a publisher's retry behaviour can be observed.
func (c *capturedStream) failNextAnnounces(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failAnnounces = n
}

func (c *capturedStream) description() *description.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.desc
}

func (c *capturedStream) packets() []*rtp.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*rtp.Packet(nil), c.pkts...)
}

func (c *capturedStream) announceCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.announces
}

type testServerHandler struct {
	cap *capturedStream
}

func (h *testServerHandler) OnAnnounce(
	ctx *gortsplib.ServerHandlerOnAnnounceCtx,
) (*base.Response, error) {
	h.cap.mu.Lock()
	h.cap.announces++
	if h.cap.failAnnounces > 0 {
		h.cap.failAnnounces--
		h.cap.mu.Unlock()
		return &base.Response{StatusCode: base.StatusServiceUnavailable},
			errors.New("announce rejected")
	}
	h.cap.desc = ctx.Description
	h.cap.mu.Unlock()
	h.cap.announceOnce.Do(func() { close(h.cap.announce) })
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h *testServerHandler) OnSetup(
	_ *gortsplib.ServerHandlerOnSetupCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil, nil
}

func (h *testServerHandler) OnRecord(
	ctx *gortsplib.ServerHandlerOnRecordCtx,
) (*base.Response, error) {
	ctx.Session.OnPacketRTPAny(func(_ *description.Media, _ format.Format, pkt *rtp.Packet) {
		h.cap.mu.Lock()
		h.cap.pkts = append(h.cap.pkts, pkt)
		h.cap.mu.Unlock()
	})
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h *testServerHandler) OnPacketsLost(_ *gortsplib.ServerHandlerOnPacketsLostCtx) {}

func startTestRTSPServer(t *testing.T) (string, *capturedStream) {
	t.Helper()

	cap := &capturedStream{announce: make(chan struct{})}

	// Bind an ephemeral port and read the real address back from the server's
	// own listener. Probing for a free port and rebinding it would leave a
	// window for another process to take it.
	s := &gortsplib.Server{
		Handler:     &testServerHandler{cap: cap},
		RTSPAddress: "127.0.0.1:0",
	}
	require.NoError(t, s.Start())
	t.Cleanup(s.Close)

	return "rtsp://" + s.NetListener().Addr().String(), cap
}

func TestPublisherForwardsVideoPackets(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Front doorbell"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	const sourceSSRC = 12345
	pkt := &rtp.Packet{
		Header: rtp.Header{
			PayloadType:    96,
			SequenceNumber: 7,
			Timestamp:      9000,
			SSRC:           sourceSSRC,
		},
		Payload: []byte{0x65, 0x01, 0x02},
	}
	require.NoError(t, p.WriteRTP(videoTrack, pkt))

	select {
	case <-cap.announce:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received ANNOUNCE")
	}

	require.Eventually(t, func() bool {
		return len(cap.packets()) > 0
	}, 2*time.Second, 10*time.Millisecond)

	got := cap.packets()[0]
	assert.Equal(t, uint16(7), got.SequenceNumber)
	assert.Equal(t, uint32(9000), got.Timestamp)
	assert.Equal(t, []byte{0x65, 0x01, 0x02}, got.Payload)
	// gortsplib mutates the caller's packet in place to use the RTSP session's
	// local SSRC. Sequence number, timestamp, and payload remain unchanged.
	assert.NotEqual(t, uint32(sourceSSRC), got.SSRC)
}

func TestPublisherAnnouncesVideoOnly(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Backyard"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	}))

	select {
	case <-cap.announce:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received ANNOUNCE")
	}

	desc := cap.description()
	require.Len(t, desc.Medias, 1)
	assert.Equal(t, description.MediaTypeVideo, desc.Medias[0].Type)
	assert.IsType(t, &format.H264{}, desc.Medias[0].Formats[0])
}

func TestPublisherAnnouncesVideoAndAudio(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(
		config.Camera{Name: "Front doorbell", Audio: true},
		baseURL,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	}))

	select {
	case <-cap.announce:
		t.Fatal("announced before the audio track arrived")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, p.WriteRTP(audioTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 111},
		Payload: []byte{0xfc},
	}))

	select {
	case <-cap.announce:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received ANNOUNCE")
	}

	desc := cap.description()
	require.Len(t, desc.Medias, 2)
	assert.Equal(t, description.MediaTypeVideo, desc.Medias[0].Type)
	assert.IsType(t, &format.H264{}, desc.Medias[0].Formats[0])
	assert.Equal(t, description.MediaTypeAudio, desc.Medias[1].Type)
	opus, ok := desc.Medias[1].Formats[0].(*format.Opus)
	require.True(t, ok)
	assert.Equal(t, 2, opus.ChannelCount)
}

func TestPublisherDropsAudioWhenDisabled(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Backyard"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	// Google requires an audio m-line in every offer, so an Opus track arrives
	// even for a camera configured without audio. Discarding it is normal and
	// must not be reported as an error.
	require.NoError(t, p.WriteRTP(audioTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 111},
		Payload: []byte{0xfc},
	}))
	assert.Equal(t, uint64(0), p.SinkStats().Failed)
	assert.Equal(t, uint64(1), p.SinkStats().Discarded)
	// Intentional discards must not be mistaken for lost media.
	assert.Equal(t, uint64(0), p.SinkStats().QueueDropped)

	require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	}))

	select {
	case <-cap.announce:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received ANNOUNCE")
	}

	desc := cap.description()
	require.Len(t, desc.Medias, 1)
	assert.Equal(t, description.MediaTypeVideo, desc.Medias[0].Type)
	require.Eventually(t, func() bool {
		return len(cap.packets()) == 1
	}, 2*time.Second, 10*time.Millisecond)
}

func TestPublisherAnnouncesVideoWhenAudioNeverArrives(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(
		config.Camera{Name: "Front doorbell", Audio: true},
		baseURL,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	p.SetAnnounceGrace(20 * time.Millisecond)
	require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	}))

	select {
	case <-cap.announce:
	case <-time.After(2 * time.Second):
		t.Fatal("grace period expired but video was never announced")
	}

	desc := cap.description()
	require.Len(t, desc.Medias, 1)
	assert.Equal(t, description.MediaTypeVideo, desc.Medias[0].Type)
	assert.Equal(t, 1, cap.announceCount())

	// ANNOUNCE is only the first of four round trips, and packets arriving
	// before the last one are dropped, so keep offering until one lands.
	require.Eventually(t, func() bool {
		_ = p.WriteRTP(videoTrack, &rtp.Packet{
			Header:  rtp.Header{PayloadType: 96, SequenceNumber: 2},
			Payload: []byte{0x65, 0x02},
		})
		return len(cap.packets()) > 0
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []byte{0x65, 0x02}, cap.packets()[0].Payload)
	assert.Equal(t, 1, cap.announceCount())
}

func TestPublisherWaitsForVideoAfterAudioOnlyGraceExpiry(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(
		config.Camera{Name: "Front doorbell", Audio: true},
		baseURL,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	p.SetAnnounceGrace(10 * time.Millisecond)
	require.NoError(t, p.WriteRTP(audioTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 111},
		Payload: []byte{0xfc},
	}))

	select {
	case <-cap.announce:
		t.Fatal("announced an audio-only session")
	case <-time.After(30 * time.Millisecond):
	}

	require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	}))

	select {
	case <-cap.announce:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received ANNOUNCE after video arrived")
	}

	desc := cap.description()
	require.Len(t, desc.Medias, 2)
	assert.Equal(t, description.MediaTypeVideo, desc.Medias[0].Type)
	assert.Equal(t, description.MediaTypeAudio, desc.Medias[1].Type)
	assert.Equal(t, 1, cap.announceCount())
}

func TestPublisherAnnouncesExactlyOnceWhenGraceRacesTrackArrival(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(
		config.Camera{Name: "Front doorbell", Audio: true},
		baseURL,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	const grace = time.Millisecond
	p.SetAnnounceGrace(grace)
	require.NoError(t, p.WriteRTP(audioTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 111},
		Payload: []byte{0xfc},
	}))

	writeResult := make(chan error, 1)
	time.AfterFunc(grace, func() {
		writeResult <- p.WriteRTP(videoTrack, &rtp.Packet{
			Header:  rtp.Header{PayloadType: 96},
			Payload: []byte{0x65},
		})
	})

	require.NoError(t, <-writeResult)
	select {
	case <-cap.announce:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received ANNOUNCE")
	}

	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 1, cap.announceCount())
}

func TestPublisherRejectsNonOpusAudio(t *testing.T) {
	baseURL, _ := startTestRTSPServer(t)
	p, err := media.NewPublisher(
		config.Camera{Name: "Front doorbell", Audio: true},
		baseURL,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	pcmu := session.TrackInfo{
		Kind:     webrtc.RTPCodecTypeAudio,
		MimeType: webrtc.MimeTypePCMU,
	}
	err = p.WriteRTP(pcmu, &rtp.Packet{Header: rtp.Header{PayloadType: 0}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestPublisherCloseBeforeAnnounceIsSafe(t *testing.T) {
	baseURL, _ := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Backyard"}, baseURL)
	require.NoError(t, err)

	assert.NoError(t, p.Close())
	assert.NoError(t, p.Close(), "Close must be idempotent")
}

func TestPublisherRejectsUnsupportedCodecBeforeAnnounce(t *testing.T) {
	baseURL, _ := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Backyard"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	vp8 := session.TrackInfo{
		Kind:     webrtc.RTPCodecTypeVideo,
		MimeType: webrtc.MimeTypeVP8,
	}
	err = p.WriteRTP(vp8, &rtp.Packet{Header: rtp.Header{PayloadType: 96}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestNewPublisherRejectsFragmentURL(t *testing.T) {
	var err error
	require.NotPanics(t, func() {
		_, err = media.NewPublisher(
			config.Camera{Name: "Backyard"},
			"rtsp://sensitive-user:top-secret@127.0.0.1:8554#note",
		)
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "sensitive-user")
	assert.NotContains(t, err.Error(), "top-secret")
}

func TestNewPublisherRejectsOpaqueURL(t *testing.T) {
	_, err := media.NewPublisher(config.Camera{Name: "Backyard"}, "rtsp:opaque")
	require.Error(t, err)
}

func TestPublisherRejectsPayloadTypeChange(t *testing.T) {
	baseURL, _ := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Driveway"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96, SequenceNumber: 1},
		Payload: []byte{0x65},
	}))

	var writeErr error
	require.NotPanics(t, func() {
		writeErr = p.WriteRTP(videoTrack, &rtp.Packet{
			Header:  rtp.Header{PayloadType: 97, SequenceNumber: 2},
			Payload: []byte{0x65},
		})
	})
	require.Error(t, writeErr)
	assert.Contains(t, writeErr.Error(), "payload type")
}

func TestPublisherAnnouncesExactlyOnce(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Driveway"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	for sequence := uint16(1); sequence <= 2; sequence++ {
		require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
			Header:  rtp.Header{PayloadType: 96, SequenceNumber: sequence},
			Payload: []byte{0x65},
		}))
	}

	assert.Equal(t, 1, cap.announceCount())
}

func TestPublisherSupportsConcurrentWrites(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Driveway"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	// Announce first. Packets offered while the RTSP session is still being
	// set up are dropped rather than queued behind it, so the concurrency
	// under test here is the steady-state write path.
	require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96, SequenceNumber: 0},
		Payload: []byte{0x65},
	}))
	require.Equal(t, uint64(1), p.SinkStats().Published)

	const packetCount = 24
	var wg sync.WaitGroup
	errs := make(chan error, packetCount)
	for i := range packetCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.WriteRTP(videoTrack, &rtp.Packet{
				Header: rtp.Header{
					PayloadType:    96,
					SequenceNumber: 1,
					Timestamp:      uint32(i * 3000),
					SSRC:           uint32(i + 1),
				},
				Payload: []byte{0x65, byte(i)},
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return len(cap.packets()) == packetCount+1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, uint64(packetCount+1), p.SinkStats().Published)
}

func TestPublisherErrorsRedactURLCredentials(t *testing.T) {
	p, err := media.NewPublisher(
		config.Camera{Name: "Backyard"},
		"rtsp://sensitive-user:top-secret@127.0.0.1:1",
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	p.SetAnnounceRetryDelay(time.Millisecond)

	err = p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "sensitive-user")
	assert.NotContains(t, err.Error(), "top-secret")
	assert.Contains(t, err.Error(), fmt.Sprintf("127.0.0.1:%d", 1))
}

// A failed announce is fatal for the connection, and says so, so that the
// session manager tears the connection down instead of streaming into nothing.
func TestPublisherFailedAnnounceIsFatalAndWrapsCause(t *testing.T) {
	p, err := media.NewPublisher(config.Camera{Name: "Backyard"}, "rtsp://127.0.0.1:1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	p.SetAnnounceRetryDelay(time.Millisecond)

	err = p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, session.ErrSinkFatal)
	assert.Contains(t, err.Error(), "connection refused")
	assert.Equal(t, uint64(0), p.SinkStats().Published)
	assert.Equal(t, uint64(1), p.SinkStats().Failed)
}

// A MediaMTX restart, or a bridge that starts before MediaMTX binds its port,
// must cost a short delay rather than the whole run.
func TestPublisherRetriesFailedAnnounce(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	cap.failNextAnnounces(2)
	p, err := media.NewPublisher(config.Camera{Name: "Backyard"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	p.SetAnnounceRetryDelay(time.Millisecond)

	require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	}))

	select {
	case <-cap.announce:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a successful ANNOUNCE")
	}
	assert.Equal(t, 3, cap.announceCount())
	assert.Equal(t, uint64(1), p.SinkStats().Published)
}

func TestPublisherCountsPublishedPackets(t *testing.T) {
	baseURL, cap := startTestRTSPServer(t)
	p, err := media.NewPublisher(config.Camera{Name: "Backyard"}, baseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	for sequence := uint16(1); sequence <= 3; sequence++ {
		require.NoError(t, p.WriteRTP(videoTrack, &rtp.Packet{
			Header:  rtp.Header{PayloadType: 96, SequenceNumber: sequence},
			Payload: []byte{0x65},
		}))
	}

	require.Eventually(t, func() bool {
		return len(cap.packets()) == 3
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, session.SinkStats{Published: 3}, p.SinkStats())
}

// sinkGuard reproduces the discipline of the unexported guardedSink the session
// manager wraps every publisher in: an atomic closed flag checked before each
// write, and a Close that flips it and closes the inner sink without waiting
// for writes already in flight. Driving the publisher through it keeps this
// test honest about the shipped wiring, which calling Publisher.Close directly
// would not be.
//
// That the real guard has this discipline is asserted by
// TestGuardedSinkCloseIsNotBlockedByInFlightWrite in internal/session. The two
// tests cannot be merged: internal/media imports internal/session, so a session
// test cannot reach a real publisher without an import cycle.
type sinkGuard struct {
	closed    atomic.Bool
	closeOnce sync.Once
	inner     *media.Publisher
}

func (g *sinkGuard) WriteRTP(info session.TrackInfo, pkt *rtp.Packet) error {
	if g.closed.Load() {
		return nil
	}
	return g.inner.WriteRTP(info, pkt)
}

func (g *sinkGuard) Close() error {
	g.closed.Store(true)
	var err error
	g.closeOnce.Do(func() { err = g.inner.Close() })
	return err
}

// Announce runs without the publisher mutex, so teardown never queues behind
// its round trips, and a pending retry is abandoned as soon as Close lands —
// through the guard, which is how the manager closes a sink.
func TestPublisherCloseInterruptsAnnounceRetry(t *testing.T) {
	p, err := media.NewPublisher(config.Camera{Name: "Backyard"}, "rtsp://127.0.0.1:1")
	require.NoError(t, err)
	p.SetAnnounceRetryDelay(time.Minute)
	guard := &sinkGuard{inner: p}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- guard.WriteRTP(videoTrack, &rtp.Packet{
			Header:  rtp.Header{PayloadType: 96},
			Payload: []byte{0x65},
		})
	}()

	// Let the write reach its first announce attempt, which is refused at once,
	// leaving it waiting out the retry delay while it holds the guard read lock.
	time.Sleep(100 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- guard.Close() }()

	select {
	case closeErr := <-closed:
		require.NoError(t, closeErr)
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind the in-flight write and its announce")
	}
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("announce retry outlived Close")
	}

	// The guard's contract survives: a write starting after Close is a no-op.
	require.NoError(t, guard.WriteRTP(videoTrack, &rtp.Packet{
		Header:  rtp.Header{PayloadType: 96},
		Payload: []byte{0x65},
	}))
}
