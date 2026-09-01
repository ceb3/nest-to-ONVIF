package sdm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	generateWebRTCStreamCommand = "sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream"
	extendWebRTCStreamCommand   = "sdm.devices.commands.CameraLiveStream.ExtendWebRtcStream"
	stopWebRTCStreamCommand     = "sdm.devices.commands.CameraLiveStream.StopWebRtcStream"
)

// ErrExtendUnsupported reports that a device refuses ExtendWebRtcStream. Battery
// doorbells are believed to refuse permanently, so the caller should regenerate rather
// than retry. That behaviour is inferred and awaits live verification; see the mapping
// in mapStreamCommandError.
var ErrExtendUnsupported = errors.New("sdm: stream extension not supported for this device")

type StreamSession struct {
	AnswerSDP      string
	MediaSessionID string
	ExpiresAt      time.Time
}

type commandRequest struct {
	Command string `json:"command"`
	Params  any    `json:"params"`
}

type streamResults struct {
	Results struct {
		AnswerSDP      string `json:"answerSdp"`
		MediaSessionID string `json:"mediaSessionId"`
		ExpiresAt      string `json:"expiresAt"`
	} `json:"results"`
}

func (c *Client) executeCommand(ctx context.Context, deviceID string, req commandRequest, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode command: %w", err)
	}
	u := fmt.Sprintf("%s/%s:executeCommand", c.baseURL, deviceID)
	return c.do(ctx, http.MethodPost, u, bytes.NewReader(body), out)
}

func parseSession(r streamResults) (*StreamSession, error) {
	expiresAt, err := time.Parse(time.RFC3339, r.Results.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse expiresAt %q: %w", r.Results.ExpiresAt, err)
	}
	return &StreamSession{
		AnswerSDP:      r.Results.AnswerSDP,
		MediaSessionID: r.Results.MediaSessionID,
		ExpiresAt:      expiresAt,
	}, nil
}

// GenerateWebRTCStream opens a live stream. The answer must be applied within 30
// seconds or it expires and the command must be repeated.
func (c *Client) GenerateWebRTCStream(ctx context.Context, deviceID, offerSDP string) (*StreamSession, error) {
	var out streamResults
	err := c.executeCommand(ctx, deviceID, commandRequest{
		Command: generateWebRTCStreamCommand,
		Params:  map[string]string{"offerSdp": offerSDP},
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("generate webrtc stream: %w", mapStreamCommandError(generateWebRTCStreamCommand, err))
	}
	return parseSession(out)
}

func (c *Client) ExtendWebRTCStream(ctx context.Context, deviceID, mediaSessionID string) (*StreamSession, error) {
	var out streamResults
	err := c.executeCommand(ctx, deviceID, commandRequest{
		Command: extendWebRTCStreamCommand,
		Params:  map[string]string{"mediaSessionId": mediaSessionID},
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("extend webrtc stream: %w", mapStreamCommandError(extendWebRTCStreamCommand, err))
	}
	s, err := parseSession(out)
	if err != nil {
		return nil, err
	}
	if s.MediaSessionID == "" {
		s.MediaSessionID = mediaSessionID
	}
	return s, nil
}

// mapStreamCommandError is the single response-classification point for stream
// commands. No real SDM failures have been observed yet: the unsupported mapping
// is inferred from Google's example wording, while bare FAILED_PRECONDITION is
// deliberately left unclassified because it may instead mean an expired session.
// NOT_FOUND is treated as already gone only for Stop; that idempotency behavior is
// intentional, but the status mapping still requires confirmation in live tests.
func mapStreamCommandError(command string, err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}

	switch {
	case command == extendWebRTCStreamCommand &&
		apiErr.Status == "FAILED_PRECONDITION" &&
		strings.Contains(strings.ToLower(apiErr.Message), "command is not supported"):
		return fmt.Errorf("%w: %w", ErrExtendUnsupported, err)
	case command == stopWebRTCStreamCommand && apiErr.Status == "NOT_FOUND":
		return nil
	default:
		return err
	}
}

// StopWebRTCStream is idempotent: if Google reports that the session no longer
// exists, the desired stopped state has already been reached and this returns nil.
func (c *Client) StopWebRTCStream(ctx context.Context, deviceID, mediaSessionID string) error {
	err := c.executeCommand(ctx, deviceID, commandRequest{
		Command: stopWebRTCStreamCommand,
		Params:  map[string]string{"mediaSessionId": mediaSessionID},
	}, nil)
	if err == nil {
		return nil
	}
	if err = mapStreamCommandError(stopWebRTCStreamCommand, err); err != nil {
		return fmt.Errorf("stop webrtc stream: %w", err)
	}
	return nil
}
