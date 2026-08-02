// Package flowspeech provides a small Go client for the FlowSpeech text-to-speech API.
package flowspeech

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://flowspeech.io"

// Client calls the FlowSpeech API.
type Client struct {
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
}

// Option customizes a Client.
type Option func(*Client) error

// WithBaseURL replaces the default API base URL. It is primarily useful for tests.
func WithBaseURL(rawURL string) Option {
	return func(c *Client) error {
		u, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("parse base URL: %w", err)
		}
		if u.Scheme == "" || u.Host == "" {
			return errors.New("base URL must include a scheme and host")
		}
		c.baseURL = u
		return nil
	}
}

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) error {
		if httpClient == nil {
			return errors.New("HTTP client cannot be nil")
		}
		c.httpClient = httpClient
		return nil
	}
}

// NewClient creates a FlowSpeech API client. The API key is sent only as a Bearer token.
func NewClient(apiKey string, options ...Option) (*Client, error) {
	baseURL, _ := url.Parse(defaultBaseURL)
	c := &Client{
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(apiKey),
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}

	for _, option := range options {
		if err := option(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Speaker assigns a FlowSpeech voice to an optional speaker label.
type Speaker struct {
	Speaker   string `json:"speaker,omitempty"`
	VoiceName string `json:"voiceName"`
}

// SynthesisRequest describes a single-speaker or multi-speaker request.
type SynthesisRequest struct {
	Text         string    `json:"text"`
	OriginalText string    `json:"originalText,omitempty"`
	Speakers     []Speaker `json:"speakers"`
}

// Quota describes the current account allowance.
type Quota struct {
	Limit     int        `json:"limit"`
	Used      int        `json:"used"`
	Remaining int        `json:"remaining"`
	ResetAt   *time.Time `json:"resetAt,omitempty"`
	IsGuest   bool       `json:"isGuest"`
}

// Audio contains generated audio and format metadata.
type Audio struct {
	MIMEType      string `json:"mimeType"`
	AudioBase64   string `json:"audioBase64"`
	Quota         *Quota `json:"quota,omitempty"`
	SampleRate    int    `json:"sampleRate"`
	NumChannels   int    `json:"numChannels"`
	BitsPerSample int    `json:"bitsPerSample"`
}

// Bytes decodes the generated base64 audio.
func (a Audio) Bytes() ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(a.AudioBase64)
	if err != nil {
		return nil, fmt.Errorf("decode audio: %w", err)
	}
	return data, nil
}

// APIError is returned when FlowSpeech rejects a request.
type APIError struct {
	StatusCode int
	Code       int
	Message    string
	ErrorCode  string
	Retryable  bool
	RetryAfter string
}

func (e *APIError) Error() string {
	if e.ErrorCode != "" {
		return fmt.Sprintf("flowspeech: %s (%s, HTTP %d)", e.Message, e.ErrorCode, e.StatusCode)
	}
	return fmt.Sprintf("flowspeech: %s (HTTP %d)", e.Message, e.StatusCode)
}

type envelope[T any] struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type errorData struct {
	ErrorCode string `json:"errorCode"`
	Retryable bool   `json:"retryable"`
}

// Quota returns the authenticated account's current TTS quota.
func (c *Client) Quota(ctx context.Context) (*Quota, error) {
	var response envelope[struct {
		Quota Quota `json:"quota"`
	}]
	if err := c.do(ctx, http.MethodGet, "/api/ai/text-to-speech/quota", nil, &response); err != nil {
		return nil, err
	}
	return &response.Data.Quota, nil
}

// Synthesize generates speech from text and one or more voice assignments.
func (c *Client) Synthesize(ctx context.Context, request SynthesisRequest) (*Audio, error) {
	if strings.TrimSpace(request.Text) == "" {
		return nil, errors.New("flowspeech: text is required")
	}
	if len(request.Speakers) == 0 {
		request.Speakers = []Speaker{{VoiceName: "Kore"}}
	}
	for _, speaker := range request.Speakers {
		if strings.TrimSpace(speaker.VoiceName) == "" {
			return nil, errors.New("flowspeech: every speaker requires a voice name")
		}
	}
	if request.OriginalText == "" {
		request.OriginalText = request.Text
	}

	var response envelope[Audio]
	if err := c.do(ctx, http.MethodPost, "/api/ai/text-to-speech", request, &response); err != nil {
		return nil, err
	}
	if response.Data.AudioBase64 == "" {
		return nil, errors.New("flowspeech: response did not include audio")
	}
	return &response.Data, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, output any) error {
	if c == nil || c.baseURL == nil || c.httpClient == nil {
		return errors.New("flowspeech: client is not initialized")
	}
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: path})

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure envelope[errorData]
		_ = json.Unmarshal(responseBody, &failure)
		message := failure.Message
		if message == "" {
			message = strings.TrimSpace(string(responseBody))
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return &APIError{
			StatusCode: resp.StatusCode,
			Code:       failure.Code,
			Message:    message,
			ErrorCode:  failure.Data.ErrorCode,
			Retryable:  failure.Data.Retryable,
			RetryAfter: resp.Header.Get("Retry-After"),
		}
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
