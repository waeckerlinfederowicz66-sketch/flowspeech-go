// Package flowspeech implements the server-side FlowSpeech public API v1.
package flowspeech

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Version = "0.2.1"
const defaultBaseURL = "https://flowspeech.io"
const jobsPath = "/api/v1/tts/jobs"

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var errorTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Client is safe for concurrent use. Injected transports must be trusted: they
// must not add credentials, cookies, or follow redirects themselves.
type Client struct {
	baseURL                    *url.URL
	apiKey                     string
	httpClient, downloadClient *http.Client
	timeout, pollInterval      time.Duration
	maxRetries                 int
	maxDownloadBytes           int64
	sleep                      func(context.Context, time.Duration) error
}
type Option func(*Client) error

// WithBaseURL accepts HTTPS, or HTTP loopback URLs for local tests.
func WithBaseURL(raw string) Option {
	return func(c *Client) error {
		u, err := safeURL(raw)
		if err != nil {
			return err
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return errors.New("base URL must be an origin")
		}
		c.baseURL = u
		return nil
	}
}

// WithHTTPClient configures API requests only. Its cookie jar is removed and
// redirects disabled on a copy; the caller's client is not modified.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) error {
		if h == nil {
			return errors.New("nil HTTP client")
		}
		c.httpClient = h
		return nil
	}
}

// WithDownloadHTTPClient configures the separate, credential-free audio client.
func WithDownloadHTTPClient(h *http.Client) Option {
	return func(c *Client) error {
		if h == nil {
			return errors.New("nil download client")
		}
		c.downloadClient = h
		return nil
	}
}

// WithTimeout bounds each operation, including polling and downloading.
// An earlier context deadline always wins.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) error {
		if d <= 0 {
			return errors.New("timeout must be positive")
		}
		c.timeout = d
		return nil
	}
}
func WithPollInterval(d time.Duration) Option {
	return func(c *Client) error {
		if d < 5*time.Second {
			return errors.New("poll interval must be at least five seconds")
		}
		c.pollInterval = d
		return nil
	}
}
func WithMaxRetries(n int) Option {
	return func(c *Client) error {
		if n < 0 || n > 10 {
			return errors.New("max retries must be 0..10")
		}
		c.maxRetries = n
		return nil
	}
}
func WithMaxDownloadBytes(n int64) Option {
	return func(c *Client) error {
		if n <= 0 || n > 1<<30 {
			return errors.New("download limit must be 1..1073741824 bytes")
		}
		c.maxDownloadBytes = n
		return nil
	}
}
func NewClient(apiKey string, options ...Option) (*Client, error) {
	if apiKey == "" || strings.ContainsAny(apiKey, " \t\r\n") {
		return nil, errors.New("a nonempty API key without whitespace is required")
	}
	u, _ := url.Parse(defaultBaseURL)
	c := &Client{baseURL: u, apiKey: apiKey, httpClient: &http.Client{}, downloadClient: &http.Client{}, timeout: 10 * time.Minute, pollInterval: 5 * time.Second, maxRetries: 2, maxDownloadBytes: 64 << 20, sleep: wait}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil option")
		}
		if err := option(c); err != nil {
			return nil, err
		}
	}
	c.httpClient = isolatedClient(c.httpClient)
	c.downloadClient = isolatedClient(c.downloadClient)
	return c, nil
}
func isolatedClient(h *http.Client) *http.Client {
	copy := *h
	copy.Jar = nil
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copy.Timeout <= 0 || copy.Timeout > 30*time.Second {
		copy.Timeout = 30 * time.Second
	}
	return &copy
}

type Speaker struct {
	Speaker   string `json:"speaker,omitempty"`
	VoiceName string `json:"voiceName"`
}
type Segment struct {
	Speaker   string `json:"speaker"`
	Text      string `json:"text"`
	VoiceName string `json:"voiceName"`
}

// SynthesisRequest retains the legacy single-voice signature. For dialogue use
// Segments, not a labelled Text script. OriginalText is unsupported.
type SynthesisRequest struct {
	Text, OriginalText string
	Speakers           []Speaker
	Segments           []Segment
	RequestID          string
}

// Quota retains legacy fields for source compatibility; v1 only populates
// Remaining, Reserved, Available, Entitlement/Subscription and Limits.
type Quota struct {
	Remaining    int             `json:"remaining"`
	Reserved     int             `json:"reserved"`
	Available    int             `json:"available"`
	Entitlement  json.RawMessage `json:"-"`
	Subscription json.RawMessage `json:"-"`
	Limits       json.RawMessage `json:"-"`
	Limit        int             `json:"-"`
	Used         int             `json:"-"`
	ResetAt      *time.Time      `json:"-"`
	IsGuest      bool            `json:"-"`
}
type Voice struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Gender      string `json:"gender"`
}
type Usage struct {
	BillableCharacters int `json:"billableCharacters"`
	ReservedCredits    int `json:"reservedCredits"`
	ChargedCredits     int `json:"chargedCredits"`
}
type Result struct {
	URL           string `json:"url"`
	MIMEType      string `json:"mimeType"`
	SampleRate    int    `json:"sampleRate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bitsPerSample"`
}
type JobFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Job struct {
	ID            string      `json:"id"`
	RequestID     string      `json:"requestId"`
	Status        string      `json:"status"`
	Result        *Result     `json:"result"`
	ResultDeleted bool        `json:"resultDeleted"`
	Usage         Usage       `json:"usage"`
	Error         *JobFailure `json:"error"`
	RetryAfter    string      `json:"-"`
}

// AudioBase64 is encoded locally from downloaded WAV bytes, never returned by v1.
type Audio struct {
	MIMEType      string `json:"mimeType"`
	AudioBase64   string `json:"audioBase64"`
	Quota         *Quota `json:"quota,omitempty"`
	SampleRate    int    `json:"sampleRate"`
	NumChannels   int    `json:"numChannels"`
	BitsPerSample int    `json:"bitsPerSample"`
	JobID         string `json:"jobId"`
	RequestID     string `json:"requestId"`
	Usage         Usage  `json:"usage"`
}

func (a Audio) Bytes() ([]byte, error) { return base64.StdEncoding.DecodeString(a.AudioBase64) }

type APIError struct {
	StatusCode                              int
	Code                                    int // Deprecated: use ErrorCode for v1 string codes.
	Message, ErrorCode, RetryAfter, TraceID string
	Retryable                               bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("flowspeech: %s (%s, HTTP %d)", e.Message, e.ErrorCode, e.StatusCode)
}

// OperationError preserves recovery identifiers even after a lost create response.
type OperationError struct {
	RequestID, JobID string
	Err              error
}

func (e *OperationError) Error() string {
	return fmt.Sprintf("flowspeech request %s job %s: %v", e.RequestID, e.JobID, e.Err)
}
func (e *OperationError) Unwrap() error { return e.Err }
func NewRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func (c *Client) Quota(ctx context.Context) (*Quota, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var out struct {
		Quota        *Quota          `json:"quota"`
		Entitlement  json.RawMessage `json:"entitlement"`
		Subscription json.RawMessage `json:"subscription"`
		Limits       json.RawMessage `json:"limits"`
	}
	_, err := c.request(ctx, "GET", "/api/v1/quota", nil, "", &out)
	if err != nil {
		return nil, err
	}
	if out.Quota == nil {
		return nil, errors.New("missing quota")
	}
	out.Quota.Entitlement, out.Quota.Subscription, out.Quota.Limits = out.Entitlement, out.Subscription, out.Limits
	return out.Quota, nil
}
func (c *Client) Voices(ctx context.Context) ([]Voice, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var out struct {
		Voices []Voice `json:"voices"`
	}
	_, err := c.request(ctx, "GET", "/api/v1/voices", nil, "", &out)
	if err == nil && out.Voices == nil {
		err = errors.New("missing voices")
	}
	return out.Voices, err
}
func (r SynthesisRequest) input() (any, error) {
	if r.OriginalText != "" {
		return nil, errors.New("OriginalText is unsupported by v1")
	}
	if r.Segments != nil {
		if len(r.Segments) == 0 || r.Text != "" || len(r.Speakers) > 0 {
			return nil, errors.New("use nonempty Segments without Text or Speakers")
		}
		for _, s := range r.Segments {
			if strings.TrimSpace(s.Speaker) == "" || strings.TrimSpace(s.Text) == "" || strings.TrimSpace(s.VoiceName) == "" {
				return nil, errors.New("each segment requires speaker, text and voiceName")
			}
		}
		return struct {
			Mode     string    `json:"mode"`
			Segments []Segment `json:"segments"`
		}{"multi", r.Segments}, nil
	}
	if strings.TrimSpace(r.Text) == "" {
		return nil, errors.New("text is required")
	}
	voice := "Kore"
	if len(r.Speakers) > 1 || (len(r.Speakers) == 1 && r.Speakers[0].Speaker != "") {
		return nil, errors.New("legacy labelled speakers are unsupported; use Segments")
	}
	if len(r.Speakers) == 1 {
		voice = r.Speakers[0].VoiceName
	}
	if strings.TrimSpace(voice) == "" {
		return nil, errors.New("voiceName is required")
	}
	return struct {
		Mode      string `json:"mode"`
		Text      string `json:"text"`
		VoiceName string `json:"voiceName"`
	}{"single", r.Text, voice}, nil
}
func (c *Client) CreateJob(ctx context.Context, r SynthesisRequest) (*Job, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	input, err := r.input()
	if err != nil {
		return nil, err
	}
	id := r.RequestID
	if id == "" {
		id, err = NewRequestID()
		if err != nil {
			return nil, err
		}
	}
	if !uuidPattern.MatchString(id) {
		return nil, errors.New("request ID must be a UUID")
	}
	body, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		return nil, err
	}
	j, err := c.jobRequest(ctx, "POST", jobsPath, body, id)
	if err != nil {
		return nil, &OperationError{RequestID: id, Err: err}
	}
	if j.RequestID != "" && !strings.EqualFold(j.RequestID, id) {
		return nil, &OperationError{RequestID: id, JobID: j.ID, Err: errors.New("request ID mismatch")}
	}
	j.RequestID = id
	return j, nil
}
func (c *Client) GetJob(ctx context.Context, id string) (*Job, error) { return c.byID(ctx, id, false) }
func (c *Client) CancelJob(ctx context.Context, id string) (*Job, error) {
	return c.byID(ctx, id, true)
}
func (c *Client) byID(ctx context.Context, id string, cancelJob bool) (*Job, error) {
	if !uuidPattern.MatchString(id) {
		return nil, errors.New("job ID must be a UUID")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	method, path := "GET", jobsPath+"/"+id
	if cancelJob {
		method = "POST"
		path += "/cancel"
	}
	j, err := c.jobRequest(ctx, method, path, nil, "")
	if err == nil && !strings.EqualFold(j.ID, id) {
		err = errors.New("job ID mismatch")
	}
	if err != nil {
		return nil, &OperationError{JobID: id, Err: err}
	}
	return j, nil
}
func (c *Client) FindJob(ctx context.Context, requestID string) (*Job, error) {
	if !uuidPattern.MatchString(requestID) {
		return nil, errors.New("request ID must be a UUID")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	j, err := c.jobRequest(ctx, "GET", jobsPath+"?requestId="+url.QueryEscape(requestID), nil, "")
	if err == nil && j.RequestID != "" && !strings.EqualFold(j.RequestID, requestID) {
		err = errors.New("request ID mismatch")
	}
	if err != nil {
		return nil, &OperationError{RequestID: requestID, Err: err}
	}
	j.RequestID = requestID
	return j, nil
}
func (c *Client) jobRequest(ctx context.Context, method, path string, body []byte, id string) (*Job, error) {
	var j Job
	h, err := c.request(ctx, method, path, body, id, &j)
	if err != nil {
		return nil, err
	}
	j.RetryAfter = h.Get("Retry-After")
	if err = validateJob(&j); err != nil {
		return nil, err
	}
	return &j, nil
}
func validateJob(j *Job) error {
	if j == nil || !uuidPattern.MatchString(j.ID) {
		return errors.New("missing or invalid job ID")
	}
	if j.RequestID != "" && !uuidPattern.MatchString(j.RequestID) {
		return errors.New("invalid request ID")
	}
	switch j.Status {
	case "queued", "running", "merging", "settling", "failed", "cancelled":
		return nil
	case "success":
		if j.ResultDeleted && j.Result == nil {
			return nil
		}
		if j.ResultDeleted || j.Result == nil {
			return errors.New("missing or inconsistent job result")
		}
		r := j.Result
		if r.MIMEType != "audio/wav" || r.SampleRate != 24000 || r.Channels != 1 || r.BitsPerSample != 16 {
			return errors.New("invalid WAV result metadata")
		}
		_, err := safeURL(r.URL)
		return err
	default:
		return errors.New("unknown job status")
	}
}
func (c *Client) WaitJob(ctx context.Context, j *Job) (*Job, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := validateJob(j); err != nil {
		return nil, err
	}
	requestID, jobID := j.RequestID, j.ID
	for {
		if err := ctx.Err(); err != nil {
			return nil, &OperationError{RequestID: requestID, JobID: jobID, Err: err}
		}
		var err error
		switch j.Status {
		case "success":
			if !j.ResultDeleted {
				return j, nil
			}
			err = errors.New("audio result was deleted (resultDeleted=true)")
		case "failed", "cancelled":
			code := ""
			if j.Error != nil {
				code = c.safeErrorToken(j.Error.Code, "job_failed")
			}
			err = fmt.Errorf("job %s: %s", j.Status, code)
		default:
			err = c.sleep(ctx, retryDelay(j.RetryAfter, c.pollInterval))
			if err == nil {
				j, err = c.GetJob(ctx, jobID)
			}
			if err == nil {
				if j.RequestID != "" && requestID != "" && !strings.EqualFold(j.RequestID, requestID) {
					err = errors.New("request ID mismatch")
				} else {
					j.RequestID = requestID
					continue
				}
			}
		}
		return nil, &OperationError{RequestID: requestID, JobID: jobID, Err: err}
	}
}
func (c *Client) Download(ctx context.Context, j *Job) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := validateJob(j); err != nil {
		return nil, err
	}
	if j.Status != "success" || j.ResultDeleted {
		return nil, &OperationError{RequestID: j.RequestID, JobID: j.ID, Err: errors.New("job has no downloadable result (pending, failed, cancelled or deleted)")}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", j.Result.URL, nil)
	resp, err := c.downloadClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("download HTTP %d; retry Download with the same job", resp.StatusCode)
		} else {
			var data []byte
			data, err = readBounded(resp.Body, c.maxDownloadBytes)
			if err != nil {
				err = downloadTransportError(err)
			} else {
				err = validateWAV(ctx, data)
			}
			if err == nil {
				return data, nil
			}
		}
	} else {
		err = downloadTransportError(err)
	}
	return nil, &OperationError{RequestID: j.RequestID, JobID: j.ID, Err: err}
}

func downloadTransportError(err error) error {
	// Do not retain URL-bearing transport errors, even in the unwrap chain.
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("audio download failed; retry Download with the same job")
}
func (c *Client) Synthesize(ctx context.Context, r SynthesisRequest) (*Audio, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	j, err := c.CreateJob(ctx, r)
	if err != nil {
		return nil, err
	}
	j, err = c.WaitJob(ctx, j)
	if err != nil {
		return nil, err
	}
	data, err := c.Download(ctx, j)
	if err != nil {
		return nil, err
	}
	v := j.Result
	return &Audio{MIMEType: v.MIMEType, AudioBase64: base64.StdEncoding.EncodeToString(data), SampleRate: v.SampleRate, NumChannels: v.Channels, BitsPerSample: v.BitsPerSample, JobID: j.ID, RequestID: j.RequestID, Usage: j.Usage}, nil
}
func (c *Client) request(ctx context.Context, method, path string, body []byte, id string, out any) (http.Header, error) {
	ref, _ := url.Parse(path)
	u := c.baseURL.ResolveReference(ref)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if id != "" {
			req.Header.Set("Idempotency-Key", id)
		}
		resp, err := c.httpClient.Do(req)
		delay := c.pollInterval
		if err == nil {
			var data []byte
			data, err = readBounded(resp.Body, 1<<20)
			resp.Body.Close()
			if err != nil && resp.StatusCode >= 300 && resp.StatusCode < 500 && resp.StatusCode != 429 {
				return nil, &APIError{StatusCode: resp.StatusCode, ErrorCode: "transport_error",
					Message: "API response body could not be read", RetryAfter: resp.Header.Get("Retry-After"),
					TraceID: c.safeErrorToken(resp.Header.Get("X-Request-Id"), "")}
			}
			if err == nil {
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					err = json.Unmarshal(data, out)
					return resp.Header, err
				}
				var failure struct {
					Error struct {
						Code, Message string
						Retryable     bool
					}
					TraceID string `json:"traceId"`
				}
				_ = json.Unmarshal(data, &failure)
				e := &APIError{StatusCode: resp.StatusCode, ErrorCode: c.safeErrorToken(failure.Error.Code, "api_error"), Message: "API request failed", Retryable: failure.Error.Retryable, TraceID: c.safeErrorToken(failure.TraceID, ""), RetryAfter: resp.Header.Get("Retry-After")}
				if e.TraceID == "" {
					e.TraceID = c.safeErrorToken(resp.Header.Get("X-Request-Id"), "")
				}
				if resp.StatusCode != 429 && !(resp.StatusCode >= 500 && e.Retryable) {
					return nil, e
				}
				err = e
			}
			delay = retryDelay(resp.Header.Get("Retry-After"), delay)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt >= c.maxRetries {
			return nil, err
		}
		if err = c.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}
func (c *Client) safeErrorToken(value, fallback string) string {
	if !errorTokenPattern.MatchString(value) || strings.Contains(value, c.apiKey) {
		return fallback
	}
	return value
}

func safeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
		return nil, errors.New("URL must be HTTPS without credentials or fragment (HTTP loopback allowed for tests)")
	}
	return u, nil
}
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("response exceeds byte limit")
	}
	return b, err
}
func retryDelay(raw string, minimum time.Duration) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32); err == nil && seconds >= 0 {
		if d := time.Duration(seconds) * time.Second; d > minimum {
			return d
		}
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := time.Until(t); d > minimum {
			return d
		}
	}
	return minimum
}
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
