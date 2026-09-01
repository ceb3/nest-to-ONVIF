package sdm

import "context"

// streamAPI adapts Client to the narrow interface used by the session manager.
type streamAPI struct{ c *Client }

func (s streamAPI) Generate(ctx context.Context, deviceID, offerSDP string) (*StreamSession, error) {
	return s.c.GenerateWebRTCStream(ctx, deviceID, offerSDP)
}

func (s streamAPI) Extend(ctx context.Context, deviceID, mediaSessionID string) (*StreamSession, error) {
	return s.c.ExtendWebRTCStream(ctx, deviceID, mediaSessionID)
}

func (s streamAPI) Stop(ctx context.Context, deviceID, mediaSessionID string) error {
	return s.c.StopWebRTCStream(ctx, deviceID, mediaSessionID)
}

// StreamAPIFor returns an adapter satisfying session.StreamAPI without importing the
// session package, avoiding an import cycle.
func (c *Client) StreamAPIFor() interface {
	Generate(ctx context.Context, deviceID, offerSDP string) (*StreamSession, error)
	Extend(ctx context.Context, deviceID, mediaSessionID string) (*StreamSession, error)
	Stop(ctx context.Context, deviceID, mediaSessionID string) error
} {
	return streamAPI{c: c}
}
