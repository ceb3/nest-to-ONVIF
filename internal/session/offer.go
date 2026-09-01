// Package session establishes and maintains WebRTC sessions against the SDM API.
package session

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// iceGatheringTimeout allows ample time for STUN gathering, which normally
// completes in under a second, while surfacing a broken network quickly enough
// to retry well within the five-minute session renewal cycle.
const iceGatheringTimeout = 10 * time.Second

// NewPeerConnection builds a receive-only peer connection shaped to Google's
// requirements. The caller owns the returned connection and must close it.
func NewPeerConnection(audio bool) (*webrtc.PeerConnection, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers:   []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	// Audio first: Google requires audio, video, application in that order, and
	// pion emits m-lines in transceiver order.
	if _, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add audio transceiver: %w", err)
	}
	if _, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add video transceiver: %w", err)
	}
	if _, err := pc.CreateDataChannel("dataSendChannel", nil); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("create data channel: %w", err)
	}

	return pc, nil
}

// CreateOffer produces a validated SDP offer after ICE gathering completes.
// On success the caller continues to own pc and must close it. On error,
// CreateOffer closes pc because it is no longer usable by the session.
func CreateOffer(ctx context.Context, pc *webrtc.PeerConnection) (string, error) {
	if err := ctx.Err(); err != nil {
		closePeerConnectionAsync(pc)
		return "", fmt.Errorf("create offer: %w", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return "", fmt.Errorf("create offer: %w", err)
	}
	if err := ctx.Err(); err != nil {
		closePeerConnectionAsync(pc)
		return "", fmt.Errorf("create offer: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			closePeerConnectionAsync(pc)
			return "", fmt.Errorf("set local description: %w", ctxErr)
		}
		_ = pc.Close()
		return "", fmt.Errorf("set local description: %w", err)
	}

	gatherCtx, cancel := context.WithTimeout(ctx, iceGatheringTimeout)
	defer cancel()

	select {
	case <-gatherComplete:
	case <-gatherCtx.Done():
		closePeerConnectionAsync(pc)
		return "", fmt.Errorf("gather ICE candidates: %w", gatherCtx.Err())
	}
	if err := ctx.Err(); err != nil {
		closePeerConnectionAsync(pc)
		return "", fmt.Errorf("gather ICE candidates: %w", err)
	}

	localDescription := pc.LocalDescription()
	if localDescription == nil {
		_ = pc.Close()
		return "", fmt.Errorf("gather ICE candidates: local description is unavailable")
	}

	sdp := localDescription.SDP
	if !strings.HasSuffix(sdp, "\n") {
		sdp += "\n"
	}
	if err := validateOffer(sdp); err != nil {
		_ = pc.Close()
		return "", err
	}

	return sdp, nil
}

type peerConnectionCloser interface {
	GracefulClose() error
	Close() error
}

// Start cleanup without blocking the caller. The connection closes as promptly
// as pion permits; if GracefulClose blocks, this one cleanup goroutine remains
// until pion returns because a synchronous third-party call cannot be externally
// time-bounded.
func closePeerConnectionAsync(pc peerConnectionCloser) {
	go func() {
		_ = pc.GracefulClose()
	}()
}

// validateOffer asserts Google's SDP constraints locally. It intentionally
// reports only the violated rule because SDP contains credential material.
func validateOffer(rawSDP string) (err error) {
	// Treat any parser panic caused by malformed input as a validation failure.
	// The recovered value is deliberately omitted because it could contain SDP.
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("offer SDP is malformed")
		}
	}()

	if !strings.HasSuffix(rawSDP, "\n") {
		return fmt.Errorf("offer must end with a newline")
	}

	var offer sdp.SessionDescription
	if unmarshalErr := offer.UnmarshalString(rawSDP); unmarshalErr != nil {
		return fmt.Errorf("offer SDP is malformed")
	}

	media := offer.MediaDescriptions
	mediaCounts := make(map[string]int, 3)
	for _, section := range media {
		if section != nil {
			mediaCounts[section.MediaName.Media]++
		}
	}

	switch {
	case mediaCounts["audio"] == 0:
		return fmt.Errorf("offer is missing m=audio")
	case mediaCounts["video"] == 0:
		return fmt.Errorf("offer is missing m=video")
	case mediaCounts["application"] == 0:
		return fmt.Errorf("offer is missing m=application; a data channel is required")
	case len(media) != 3:
		return fmt.Errorf("offer must contain exactly three media sections: audio, video, application")
	}

	requiredKinds := [...]string{"audio", "video", "application"}
	var mids [len(requiredKinds)]string
	for index, requiredKind := range requiredKinds {
		section := media[index]
		if section == nil || section.MediaName.Media != requiredKind {
			return fmt.Errorf("offer media sections are in the wrong order: Google requires audio, video, application")
		}

		mid, ok := singleAttributeValue(section.Attributes, "mid")
		if !ok || len(strings.Fields(mid)) != 1 {
			return fmt.Errorf("offer must use Unified Plan with one media identifier per section")
		}
		mids[index] = mid
	}

	if !bundleReferencesMIDs(offer.Attributes, mids) {
		return fmt.Errorf("offer must use Unified Plan with bundled media identifiers")
	}

	audioSection := media[0]
	videoSection := media[1]
	applicationSection := media[2]
	if !isSCTPDataChannel(applicationSection) {
		return fmt.Errorf("offer application section must be an SCTP data channel")
	}
	if !hasMappedOpusPayload(audioSection) {
		return fmt.Errorf("offer audio must include Opus")
	}
	if !hasExactDirection(audioSection, "recvonly") {
		return fmt.Errorf("offer audio must be recvonly")
	}
	if !hasExactDirection(videoSection, "recvonly") {
		return fmt.Errorf("offer video must be recvonly")
	}

	// Direction is intentionally constrained only for audio and video. SCTP
	// data channels are inherently bidirectional, so pion correctly emits
	// sendrecv in the application section; rejecting it globally would make
	// Google's mandatory data-channel and receive-only media rules impossible
	// to satisfy together.

	return nil
}

func singleAttributeValue(attributes []sdp.Attribute, key string) (string, bool) {
	var value string
	found := false
	for _, attribute := range attributes {
		if attribute.Key != key {
			continue
		}
		if found {
			return "", false
		}
		value = attribute.Value
		found = true
	}

	return value, found
}

func bundleReferencesMIDs(attributes []sdp.Attribute, mids [3]string) bool {
	expectedMIDs := make(map[string]struct{}, len(mids))
	for _, mid := range mids {
		expectedMIDs[mid] = struct{}{}
	}
	if len(expectedMIDs) != len(mids) {
		return false
	}

	bundleGroups := 0
	for _, attribute := range attributes {
		if attribute.Key != "group" {
			continue
		}

		fields := strings.Fields(attribute.Value)
		if len(fields) == 0 || fields[0] != "BUNDLE" {
			continue
		}
		bundleGroups++
		if len(fields) != len(mids)+1 {
			return false
		}
		referencedMIDs := make(map[string]struct{}, len(mids))
		for _, mid := range fields[1:] {
			if _, ok := expectedMIDs[mid]; !ok {
				return false
			}
			referencedMIDs[mid] = struct{}{}
		}
		if len(referencedMIDs) != len(expectedMIDs) {
			return false
		}
	}

	return bundleGroups == 1
}

func isSCTPDataChannel(section *sdp.MediaDescription) bool {
	if section == nil {
		return false
	}

	protocols := section.MediaName.Protos
	if len(protocols) != 3 ||
		protocols[0] != "UDP" ||
		protocols[1] != "DTLS" ||
		protocols[2] != "SCTP" {
		return false
	}

	formats := section.MediaName.Formats
	if len(formats) != 1 || formats[0] != "webrtc-datachannel" {
		return false
	}

	port, ok := singleAttributeValue(section.Attributes, "sctp-port")
	if !ok {
		return false
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	return err == nil && parsedPort != 0
}

func hasMappedOpusPayload(section *sdp.MediaDescription) bool {
	if section == nil {
		return false
	}

	formats := make(map[string]struct{}, len(section.MediaName.Formats))
	for _, format := range section.MediaName.Formats {
		formats[format] = struct{}{}
	}

	for _, attribute := range section.Attributes {
		if attribute.Key != "rtpmap" {
			continue
		}
		fields := strings.Fields(attribute.Value)
		if len(fields) != 2 {
			continue
		}
		if _, ok := formats[fields[0]]; !ok {
			continue
		}
		codecParts := strings.Split(fields[1], "/")
		if len(codecParts) != 3 || !strings.EqualFold(codecParts[0], "opus") {
			continue
		}
		clockRate, clockErr := strconv.ParseUint(codecParts[1], 10, 32)
		channels, channelsErr := strconv.ParseUint(codecParts[2], 10, 16)
		if clockErr == nil && channelsErr == nil && clockRate == 48000 && channels == 2 {
			return true
		}
	}

	return false
}

func hasExactDirection(section *sdp.MediaDescription, expected string) bool {
	direction := ""
	directionCount := 0
	for _, attribute := range section.Attributes {
		switch attribute.Key {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			direction = attribute.Key
			directionCount++
		}
	}

	return directionCount == 1 && direction == expected
}
