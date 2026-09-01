// Package media republishes WebRTC media through RTSP.
package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/session"
)

const (
	rtspDialTimeout  = 10 * time.Second
	rtspReadTimeout  = 10 * time.Second
	rtspWriteTimeout = 10 * time.Second

	defaultAnnounceGrace     = 3 * time.Second
	defaultAnnounceRetry     = 500 * time.Millisecond
	announceAttempts         = 3
	announceRetryBackoffStep = 2
)

// rtspWriter is the part of *gortsplib.Client the publisher uses. Naming it
// keeps the write and teardown paths testable without a live RTSP server.
type rtspWriter interface {
	WritePacketRTP(medi *description.Media, pkt *rtp.Packet) error
	Close()
}

// Publisher republishes RTP packets from a WebRTC session to an RTSP server.
//
// Packets are forwarded without depacketization: Nest emits H.264, which is
// what downstream consumers require, so re-encoding would cost CPU and quality
// for no benefit.
//
// A Publisher is bound to exactly one WebRTC connection. RTP identifies a
// stream by SSRC and sequence series, and a rebuilt connection produces new
// ones; reusing a Publisher across connections would present a discontinuity
// to readers.
type Publisher struct {
	url         string
	redactedURL string
	wantAudio   bool
	log         *slog.Logger

	mu         sync.Mutex
	client     rtspWriter
	desc       *description.Session
	video      *description.Media
	audio      *description.Media
	announced  bool
	announcing bool
	closed     bool

	announceGrace time.Duration
	announceRetry time.Duration
	announceTimer *time.Timer
	graceExpired  bool
	videoWaits    uint64

	// Counters are atomic so that reading them never waits on an announce.
	published    atomic.Uint64
	discarded    atomic.Uint64
	queueDropped atomic.Uint64
	failed       atomic.Uint64

	closedCh chan struct{}
}

var (
	_ session.TrackSink    = (*Publisher)(nil)
	_ session.SinkCloser   = (*Publisher)(nil)
	_ session.SinkReporter = (*Publisher)(nil)
)

// NewPublisher creates an RTSP publisher for a single camera connection.
// Network setup is deferred until the first packet reveals the available
// track set.
func NewPublisher(cam config.Camera, baseURL string) (*Publisher, error) {
	publishURL := cam.PublishURL(baseURL)
	parsed, err := url.Parse(publishURL)
	if err != nil ||
		(parsed.Scheme != "rtsp" && parsed.Scheme != "rtsps") ||
		parsed.Host == "" ||
		parsed.Fragment != "" ||
		parsed.Opaque != "" {
		return nil, errors.New("invalid RTSP publish URL")
	}

	redacted := *parsed
	redacted.User = nil

	return &Publisher{
		url:           parsed.String(),
		redactedURL:   redacted.String(),
		desc:          &description.Session{},
		wantAudio:     cam.Audio,
		log:           slog.Default().With("camera", cam.Name, "rtsp_url", redacted.String()),
		announceGrace: defaultAnnounceGrace,
		announceRetry: defaultAnnounceRetry,
		closedCh:      make(chan struct{}),
	}, nil
}

// SetAnnounceGrace changes how long the publisher waits for expected tracks.
func (p *Publisher) SetAnnounceGrace(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.announceGrace = d
	if p.announceTimer != nil && !p.announced {
		p.announceTimer.Reset(d)
	}
}

// SetAnnounceRetryDelay changes the pause between announce attempts.
func (p *Publisher) SetAnnounceRetryDelay(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.announceRetry = d
}

// SinkStats reports how much media reached the RTSP server. A run whose
// Published count stays at zero published nothing, however healthy the
// upstream WebRTC session looked.
func (p *Publisher) SinkStats() session.SinkStats {
	return session.SinkStats{
		Published:    p.published.Load(),
		Discarded:    p.discarded.Load(),
		QueueDropped: p.queueDropped.Load(),
		Failed:       p.failed.Load(),
	}
}

// WriteRTP forwards a packet to the RTSP publisher.
func (p *Publisher) WriteRTP(info session.TrackInfo, pkt *rtp.Packet) error {
	switch info.Kind {
	case webrtc.RTPCodecTypeVideo:
		if info.MimeType != webrtc.MimeTypeH264 {
			return p.fail(fmt.Errorf("unsupported RTP codec: kind=%s MIME=%q", info.Kind, info.MimeType))
		}
	case webrtc.RTPCodecTypeAudio:
		if info.MimeType != webrtc.MimeTypeOpus {
			return p.fail(fmt.Errorf("unsupported RTP codec: kind=%s MIME=%q", info.Kind, info.MimeType))
		}
	default:
		return p.fail(fmt.Errorf("unsupported RTP codec: kind=%s MIME=%q", info.Kind, info.MimeType))
	}
	if pkt == nil {
		return p.fail(errors.New("RTP packet is nil"))
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		p.discarded.Add(1)
		return nil
	}

	var medium *description.Media
	switch info.Kind {
	case webrtc.RTPCodecTypeVideo:
		medium = p.video
	case webrtc.RTPCodecTypeAudio:
		if !p.wantAudio {
			// Google requires an audio m-line in every offer, so a camera
			// configured without audio still receives an Opus track. Discarding
			// it is the configured outcome, not a failure.
			p.discarded.Add(1)
			return nil
		}
		medium = p.audio
	}

	if medium == nil {
		if p.announced || p.announcing {
			return p.fail(fmt.Errorf(
				"%s track appeared after the RTSP session was announced", info.Kind))
		}
		medium = p.addMedium(info.Kind, pkt.PayloadType)
		p.rebuildDescription()
		p.startAnnounceTimer()
	}

	if !p.announced {
		if !p.haveEveryExpectedTrack() && !p.graceExpired {
			// RTSP must announce every medium at once. A supported packet can
			// therefore arrive during the sub-second startup window before all
			// expected WebRTC tracks are known; dropping it avoids announcing
			// an incomplete session.
			p.discarded.Add(1)
			return nil
		}
		if p.announcing {
			// Another goroutine owns the announce. Its round trips run without
			// the mutex, so this one waits for the next packet rather than
			// queueing behind it.
			p.discarded.Add(1)
			return nil
		}
		if err := p.announceLocked(); err != nil {
			return p.fail(err)
		}
		if !p.announced {
			// Closed while announcing.
			p.discarded.Add(1)
			return nil
		}
	}

	if !p.mediumWasAnnounced(medium) {
		// gortsplib dereferences its per-medium state without a nil check, so
		// an unannounced medium must never reach it.
		return p.fail(fmt.Errorf(
			"%s medium is absent from the announced RTSP session description", info.Kind))
	}
	announcedPayloadType := medium.Formats[0].PayloadType()
	if pkt.PayloadType != announcedPayloadType {
		return p.fail(fmt.Errorf(
			"RTP payload type %d does not match announced payload type %d",
			pkt.PayloadType,
			announcedPayloadType,
		))
	}

	if err := p.client.WritePacketRTP(medium, pkt); err != nil {
		var queueFull liberrors.ErrClientWriteQueueFull
		if errors.As(err, &queueFull) {
			// gortsplib's outbound ring buffer is non-blocking and holds only a
			// couple of seconds of media. A brief stall must cost the packets it
			// stalls on, not the whole track. This is real loss, so it is counted
			// apart from the packets the publisher never intended to send.
			p.queueDropped.Add(1)
			return nil
		}
		return p.fail(fmt.Errorf("%w: write RTP packet to RTSP publisher for %s failed: %w",
			session.ErrSinkFatal, p.redactedURL, err))
	}
	p.published.Add(1)
	return nil
}

func (p *Publisher) fail(err error) error {
	p.failed.Add(1)
	return err
}

func (p *Publisher) addMedium(kind webrtc.RTPCodecType, payloadType uint8) *description.Media {
	switch kind {
	case webrtc.RTPCodecTypeVideo:
		p.video = &description.Media{
			Type: description.MediaTypeVideo,
			Formats: []format.Format{&format.H264{
				PayloadTyp:        payloadType,
				PacketizationMode: 1,
			}},
		}
		return p.video
	default:
		p.audio = &description.Media{
			Type: description.MediaTypeAudio,
			Formats: []format.Format{&format.Opus{
				PayloadTyp:   payloadType,
				ChannelCount: 2,
			}},
		}
		return p.audio
	}
}

func (p *Publisher) newRTSPClient() *gortsplib.Client {
	// gortsplib defaults both timeouts to ten seconds. Pinning them keeps the
	// four announce round trips bounded by a value this package controls.
	return &gortsplib.Client{
		DialContext:  p.dialContext,
		ReadTimeout:  rtspReadTimeout,
		WriteTimeout: rtspWriteTimeout,
	}
}

// dialContext connects on behalf of gortsplib, abandoning the attempt as soon
// as the publisher closes. Without this, closing could only interrupt the
// pauses between announce attempts, leaving teardown to wait out a dial to a
// host that drops packets rather than refusing them.
func (p *Publisher) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	select {
	case <-p.closedCh:
		return nil, fmt.Errorf("RTSP publisher closed: %w", context.Canceled)
	default:
	}

	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-p.closedCh:
			cancel()
		case <-dialCtx.Done():
		}
	}()

	dialer := &net.Dialer{Timeout: rtspDialTimeout}
	return dialer.DialContext(dialCtx, network, address)
}

func (p *Publisher) startAnnounceTimer() {
	if p.announceTimer != nil {
		return
	}
	p.announceTimer = time.AfterFunc(p.announceGrace, p.announceAfterGrace)
}

func (p *Publisher) announceAfterGrace() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.announced || p.announcing || p.closed {
		return
	}
	if p.video == nil {
		// Video is essential. Announcing audio alone would permanently reject
		// the video medium when it arrives, so start another grace interval.
		p.videoWaits++
		p.log.Warn("no video track yet; RTSP session still unannounced",
			"grace", p.announceGrace, "expiries", p.videoWaits)
		p.announceTimer.Reset(p.announceGrace)
		return
	}
	p.graceExpired = true
	if err := p.announceLocked(); err != nil {
		p.log.Error("RTSP announce after grace period failed", "error", err)
	}
}

// announceLocked runs ANNOUNCE, SETUP per medium, and RECORD. It is called
// with p.mu held and returns with p.mu held, but releases the mutex across the
// round trips: each is bounded by a ten-second timeout, and holding the mutex
// through all four would stall Close, and so session teardown, for up to forty
// seconds. p.announcing keeps a second announce out of that window, and no
// other path touches p.desc while it is set.
func (p *Publisher) announceLocked() error {
	p.announcing = true
	desc := p.desc
	retry := p.announceRetry
	p.mu.Unlock()

	client, err := p.announceWithRetry(desc, retry)

	p.mu.Lock()
	p.announcing = false
	if err != nil {
		return err
	}
	if p.closed {
		client.Close()
		return nil
	}
	p.client = client
	p.announced = true
	if p.announceTimer != nil {
		p.announceTimer.Stop()
	}
	return nil
}

// announceWithRetry establishes the publishing session, retrying a bounded
// number of times so that a MediaMTX restart, or a bridge that started before
// MediaMTX bound its port, costs a short delay rather than the whole run.
func (p *Publisher) announceWithRetry(
	desc *description.Session,
	retry time.Duration,
) (*gortsplib.Client, error) {
	var lastErr error
	for attempt := range announceAttempts {
		if attempt > 0 {
			select {
			case <-p.closedCh:
				return nil, fmt.Errorf(
					"%w: start RTSP publisher for %s abandoned on close: %w",
					session.ErrSinkFatal, p.redactedURL, lastErr)
			case <-time.After(retry):
			}
			retry *= announceRetryBackoffStep
		}

		client := p.newRTSPClient()
		if err := client.StartRecording(p.url, desc); err != nil {
			client.Close()
			lastErr = err
			p.log.Warn("RTSP announce failed",
				"attempt", attempt+1, "attempts", announceAttempts, "error", err)
			continue
		}
		if attempt > 0 {
			p.log.Info("RTSP announce succeeded after retry", "attempt", attempt+1)
		}
		return client, nil
	}

	return nil, fmt.Errorf("%w: start RTSP publisher for %s failed after %d attempts: %w",
		session.ErrSinkFatal, p.redactedURL, announceAttempts, lastErr)
}

func (p *Publisher) haveEveryExpectedTrack() bool {
	return p.video != nil && (!p.wantAudio || p.audio != nil)
}

func (p *Publisher) mediumWasAnnounced(medium *description.Media) bool {
	for _, announced := range p.desc.Medias {
		if announced == medium {
			return true
		}
	}
	return false
}

func (p *Publisher) rebuildDescription() {
	p.desc.Medias = p.desc.Medias[:0]
	if p.video != nil {
		p.desc.Medias = append(p.desc.Medias, p.video)
	}
	if p.audio != nil {
		p.desc.Medias = append(p.desc.Medias, p.audio)
	}
}

// Close releases the RTSP client. It is safe to call more than once.
func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	close(p.closedCh)
	if p.announceTimer != nil {
		p.announceTimer.Stop()
	}
	if p.client != nil {
		p.client.Close()
	}
	return nil
}
