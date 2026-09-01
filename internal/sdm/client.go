package sdm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	"golang.org/x/oauth2"
)

const defaultBaseURL = "https://smartdevicemanagement.googleapis.com/v1"
const requestTimeout = 30 * time.Second

// ErrRateLimited reports a 429 from the SDM API. The scheduler inspects this to decide
// whether to reduce its rate, so callers must wrap rather than replace it.
var ErrRateLimited = errors.New("sdm: rate limited")

// RateLimitError carries a rate-limit response from the SDM API. Use errors.As to read
// RetryAfter when HasRetryAfter is true; errors.Is(err, ErrRateLimited) remains valid.
type RateLimitError struct {
	Message       string
	RetryAfter    time.Duration
	HasRetryAfter bool
	apiError      *APIError
}

func (e *RateLimitError) Error() string {
	return ErrRateLimited.Error()
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// As exposes the underlying APIError while preserving RateLimitError's existing
// errors.As and ErrRateLimited behavior.
func (e *RateLimitError) As(target any) bool {
	apiTarget, ok := target.(**APIError)
	if !ok || e.apiError == nil {
		return false
	}
	*apiTarget = e.apiError
	return true
}

// APIError is a non-success response from the SDM API. Message is retained for
// programmatic classification only; Error deliberately omits Google's untrusted
// text because it may echo credentials or SDP.
type APIError struct {
	HTTPStatusCode int
	Status         string
	Message        string
}

func (e *APIError) Error() string {
	if e.Status != "" {
		return fmt.Sprintf("sdm: HTTP %d (%s)", e.HTTPStatusCode, e.Status)
	}
	return fmt.Sprintf("sdm: HTTP %d", e.HTTPStatusCode)
}

const maxListDevicePages = 100

type Client struct {
	projectID string
	baseURL   string
	http      *http.Client
}

type ClientOption func(*Client)

func WithBaseURL(u string) ClientOption { return func(c *Client) { c.baseURL = u } }

func WithHTTPClient(h *http.Client) ClientOption { return func(c *Client) { c.http = h } }

func NewClient(projectID string, ts oauth2.TokenSource, opts ...ClientOption) *Client {
	c := &Client{
		projectID: projectID,
		baseURL:   defaultBaseURL,
		http: &http.Client{
			Timeout:   requestTimeout,
			Transport: &oauth2.Transport{Source: ts},
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type Device struct {
	Name   string                     `json:"name"`
	Type   string                     `json:"type"`
	Traits map[string]json.RawMessage `json:"traits"`
}

// DisplayName returns the user-assigned name, falling back to the trailing device ID.
func (d Device) DisplayName() string {
	if raw, ok := d.Traits["sdm.devices.traits.Info"]; ok {
		var info struct {
			CustomName string `json:"customName"`
		}
		if err := json.Unmarshal(raw, &info); err == nil && info.CustomName != "" {
			return info.CustomName
		}
	}
	return path.Base(d.Name)
}

// SupportedProtocols reports the live stream formats the device accepts. A device
// listing only WEB_RTC will reject every RTSP command.
func (d Device) SupportedProtocols() []string {
	raw, ok := d.Traits["sdm.devices.traits.CameraLiveStream"]
	if !ok {
		return nil
	}
	var trait struct {
		SupportedProtocols []string `json:"supportedProtocols"`
	}
	if err := json.Unmarshal(raw, &trait); err != nil {
		return nil
	}
	return trait.SupportedProtocols
}

type apiError struct {
	Error struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) do(ctx context.Context, method, urlStr string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call sdm: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var ae apiError
		_ = json.Unmarshal(raw, &ae)
		msg := ae.Error.Message
		if msg == "" {
			msg = string(raw)
		}
		apiErr := &APIError{
			HTTPStatusCode: resp.StatusCode,
			Status:         ae.Error.Status,
			Message:        msg,
		}
		if resp.StatusCode == http.StatusTooManyRequests || ae.Error.Status == "RESOURCE_EXHAUSTED" {
			rle := &RateLimitError{Message: msg, apiError: apiErr}
			if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
				rle.RetryAfter = d
				rle.HasRetryAfter = true
			}
			return rle
		}
		return apiErr
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if sec, err := strconv.Atoi(v); err == nil {
		return time.Duration(sec) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	base := fmt.Sprintf("%s/enterprises/%s/devices", c.baseURL, c.projectID)
	var all []Device
	pageToken := ""

	for page := 0; page < maxListDevicePages; page++ {
		reqURL := base
		if pageToken != "" {
			reqURL = base + "?" + url.Values{"pageToken": {pageToken}}.Encode()
		}

		var out struct {
			Devices       []Device `json:"devices"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := c.do(ctx, http.MethodGet, reqURL, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Devices...)
		if out.NextPageToken == "" {
			return all, nil
		}
		pageToken = out.NextPageToken
	}
	return nil, fmt.Errorf("sdm: list devices exceeded %d pages", maxListDevicePages)
}
