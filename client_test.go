package flowspeech

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

const jobID = "b698824b-de05-486d-8833-05d5c4522a39"
const requestID = "51382b81-9d36-49a5-994e-9b67686c6bd1"

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string, headers ...string) *http.Response {
	h := make(http.Header)
	for i := 0; i < len(headers); i += 2 {
		h.Set(headers[i], headers[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}
func job(status string) *Job {
	j := &Job{ID: jobID, RequestID: requestID, Status: status, Usage: Usage{BillableCharacters: 5, ChargedCredits: 5}}
	if status == "success" {
		j.Result = &Result{URL: "https://audio.example.test/speech.wav", MIMEType: "audio/wav", SampleRate: 24000, Channels: 1, BitsPerSample: 16}
	}
	return j
}
func jobResponse(status string, code int) *http.Response {
	b, _ := json.Marshal(job(status))
	return response(code, string(b), "Retry-After", "7")
}
func client(t *testing.T, f transportFunc) *Client {
	t.Helper()
	c, err := NewClient("fixture-key", WithHTTPClient(&http.Client{Transport: f}))
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}
func TestSynthesizeLifecycle(t *testing.T) {
	statuses := []string{"queued", "running", "merging", "settling", "success"}
	calls := 0
	var sleeps []time.Duration
	c := client(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Fatal("missing bearer")
		}
		if calls == 0 {
			if r.Method != "POST" || r.URL.Path != jobsPath || r.Header.Get("Idempotency-Key") != requestID {
				t.Fatal("bad create")
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			want := map[string]any{"input": map[string]any{"mode": "single", "text": "Hello", "voiceName": "Kore"}}
			if !reflect.DeepEqual(body, want) {
				t.Fatalf("body %#v", body)
			}
		} else if r.Method != "GET" || r.URL.Path != jobsPath+"/"+jobID {
			t.Fatal("bad poll")
		}
		status := statuses[calls]
		calls++
		return jobResponse(status, 200), nil
	})
	c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	c.downloadClient = isolatedClient(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if len(r.Header) != 0 {
			t.Fatalf("download headers: %#v", r.Header)
		}
		return response(200, string(validWAV())), nil
	})})
	a, err := c.Synthesize(context.Background(), SynthesisRequest{Text: "Hello", RequestID: requestID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := a.Bytes()
	if err != nil || !reflect.DeepEqual(b, validWAV()) || a.JobID != jobID || a.NumChannels != 1 || a.Usage.ChargedCredits != 5 {
		t.Fatalf("bad audio %#v %v", a, err)
	}
	if calls != 5 || !reflect.DeepEqual(sleeps, []time.Duration{7 * time.Second, 7 * time.Second, 7 * time.Second, 7 * time.Second}) {
		t.Fatalf("calls %d sleeps %v", calls, sleeps)
	}
}
func TestMultiAndValidation(t *testing.T) {
	c := client(t, func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		input := body["input"].(map[string]any)
		if len(body) != 1 || len(input) != 2 || input["mode"] != "multi" || len(input["segments"].([]any)) != 2 {
			t.Fatal(body)
		}
		return jobResponse("queued", 202), nil
	})
	_, err := c.CreateJob(context.Background(), SynthesisRequest{RequestID: requestID, Segments: []Segment{{"Alice", "Hello", "Kore"}, {"Bob", "Hi", "Puck"}}})
	if err != nil {
		t.Fatal(err)
	}
	invalid := []SynthesisRequest{{}, {Text: "Hi", OriginalText: "Hi"}, {Text: "Hi", Speakers: []Speaker{{"A", "Kore"}}}, {Segments: []Segment{}}, {Text: "Hi", Segments: []Segment{{"A", "Hi", "Kore"}}}, {Text: "Hi", Speakers: []Speaker{{VoiceName: ""}}}, {Segments: []Segment{{"", "Hi", "Kore"}}}}
	for _, r := range invalid {
		if _, err := r.input(); err == nil {
			t.Fatalf("accepted %#v", r)
		}
	}
}
func TestRetryKeepsUUIDAndBody(t *testing.T) {
	var bodies, ids []string
	var sleeps []time.Duration
	c := client(t, func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		ids = append(ids, r.Header.Get("Idempotency-Key"))
		switch len(bodies) {
		case 1:
			return nil, io.ErrUnexpectedEOF
		case 2:
			return response(503, `{"error":{"code":"busy","retryable":true},"traceId":"trace"}`, "Retry-After", "12"), nil
		default:
			j := job("queued")
			j.RequestID = ids[0]
			data, _ := json.Marshal(j)
			return response(202, string(data)), nil
		}
	})
	c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	j, err := c.CreateJob(context.Background(), SynthesisRequest{Text: "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !uuidPattern.MatchString(j.RequestID) || ids[0] != ids[1] || ids[1] != ids[2] || bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatal(ids, bodies)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{5 * time.Second, 12 * time.Second}) {
		t.Fatal(sleeps)
	}
}
func TestQuotaVoicesRecoveryCancel(t *testing.T) {
	c := client(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/quota":
			return response(200, `{"quota":{"remaining":10,"reserved":3,"available":7},"entitlement":{"plan":"basic"},"subscription":{"status":"active"},"limits":{"readPerMinute":60}}`), nil
		case "/api/v1/voices":
			return response(200, `{"voices":[{"name":"Kore","gender":"female","description":"Firm"}]}`), nil
		case jobsPath:
			if r.URL.Query().Get("requestId") != requestID {
				t.Fatal(r.URL)
			}
			return jobResponse("queued", 200), nil
		case jobsPath + "/" + jobID + "/cancel":
			if r.Method != "POST" {
				t.Fatal(r.Method)
			}
			return jobResponse("cancelled", 200), nil
		}
		t.Fatal(r.URL)
		return nil, nil
	})
	q, err := c.Quota(context.Background())
	if err != nil || q.Available != 7 || len(q.Entitlement) == 0 || len(q.Subscription) == 0 || len(q.Limits) == 0 {
		t.Fatal(q, err)
	}
	v, err := c.Voices(context.Background())
	if err != nil || v[0].Name != "Kore" {
		t.Fatal(v, err)
	}
	if _, err := c.FindJob(context.Background(), requestID); err != nil {
		t.Fatal(err)
	}
	if j, err := c.CancelJob(context.Background(), jobID); err != nil || j.Status != "cancelled" {
		t.Fatal(j, err)
	}
	if _, err := c.GetJob(context.Background(), "../../evil"); err == nil {
		t.Fatal("accepted ID")
	}
}
func TestErrorsAndBoundedRetries(t *testing.T) {
	for _, status := range []int{401, 403, 409, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			c := client(t, func(*http.Request) (*http.Response, error) {
				calls++
				return response(status, `{"error":{"code":"fixture_error","message":"rejected","retryable":true},"traceId":"fixture-trace"}`, "Retry-After", "9"), nil
			})
			_, err := c.CreateJob(context.Background(), SynthesisRequest{Text: "Hi", RequestID: requestID})
			var api *APIError
			var op *OperationError
			if !errors.As(err, &api) || !errors.As(err, &op) || op.RequestID != requestID || api.TraceID != "fixture-trace" || api.ErrorCode != "fixture_error" || api.RetryAfter != "9" {
				t.Fatal(err)
			}
			want := 1
			if status == 429 || status == 503 {
				want = 3
			}
			if calls != want {
				t.Fatal(calls)
			}
		})
	}
}
func TestTerminalAndMalformed(t *testing.T) {
	c := client(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected request"); return nil, nil })
	for _, status := range []string{"failed", "cancelled"} {
		_, err := c.WaitJob(context.Background(), job(status))
		if err == nil || !strings.Contains(err.Error(), status) {
			t.Fatal(err)
		}
	}
	deleted := job("success")
	deleted.Result = nil
	deleted.ResultDeleted = true
	if _, err := c.WaitJob(context.Background(), deleted); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatal(err)
	}
	for _, j := range []*Job{nil, {ID: jobID, Status: "unknown"}, {Status: "queued"}, {ID: jobID, Status: "success"}, {ID: jobID, Status: "success", Result: &Result{URL: "https://example.test"}}} {
		if validateJob(j) == nil {
			t.Fatal(j)
		}
	}
	for _, body := range []string{`null`, `[]`, `{`, `{"status":"queued"}`, `{"id":"` + jobID + `","status":"alien"}`} {
		c := client(t, func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		if _, err := c.GetJob(context.Background(), jobID); err == nil {
			t.Fatal(body)
		}
	}
}
func TestDeadlineAndRetryAfter(t *testing.T) {
	c := client(t, func(*http.Request) (*http.Response, error) { return jobResponse("queued", 202), nil })
	c.timeout = 20 * time.Millisecond
	c.sleep = wait
	start := time.Now()
	_, err := c.Synthesize(context.Background(), SynthesisRequest{Text: "Hi", RequestID: requestID})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal(err)
	}
	d := retryDelay(time.Now().Add(20*time.Second).UTC().Format(http.TimeFormat), 5*time.Second)
	if d < 18*time.Second || d > 21*time.Second {
		t.Fatal(d)
	}
	if retryDelay("bad", 5*time.Second) != 5*time.Second || retryDelay("1", 5*time.Second) != 5*time.Second {
		t.Fatal("bad default delay")
	}
	c = client(t, func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	c.timeout = 20 * time.Millisecond
	if _, err = c.Quota(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestDownloadFailureDoesNotCreate(t *testing.T) {
	calls := 0
	c := client(t, func(*http.Request) (*http.Response, error) { calls++; return jobResponse("success", 200), nil })
	c.downloadClient = isolatedClient(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return response(503, "unavailable"), nil })})
	_, err := c.Synthesize(context.Background(), SynthesisRequest{Text: "Hi", RequestID: requestID})
	var op *OperationError
	if !errors.As(err, &op) || op.JobID != jobID || calls != 1 {
		t.Fatal(err, calls)
	}
	c.maxDownloadBytes = 2
	c.downloadClient = isolatedClient(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return response(200, "123"), nil })})
	if _, err = c.Download(context.Background(), job("success")); err == nil {
		t.Fatal("unbounded")
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

type failedDownloadBody struct{ err error }

func (b failedDownloadBody) Read([]byte) (int, error) { return 0, b.err }
func (b failedDownloadBody) Close() error             { return nil }

func TestInterruptedAuthResponseDoesNotRetry(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			c := client(t, func(*http.Request) (*http.Response, error) {
				calls++
				r := response(status, "", "Retry-After", "30")
				r.Body = failedDownloadBody{io.ErrUnexpectedEOF}
				return r, nil
			})
			c.sleep = func(context.Context, time.Duration) error { t.Fatal("auth error must not retry"); return nil }
			_, err := c.CreateJob(context.Background(), SynthesisRequest{Text: "Hi", RequestID: requestID})
			var api *APIError
			var op *OperationError
			if !errors.As(err, &api) || !errors.As(err, &op) || api.StatusCode != status || api.Retryable || api.RetryAfter != "30" || op.RequestID != requestID || calls != 1 {
				t.Fatal("lost status/recovery metadata or retried permanent error")
			}
		})
	}
}

func TestDownloadErrorsDoNotExposePrivateURLs(t *testing.T) {
	for _, stage := range []string{"transport", "body", "deadline", "cancelled", "native"} {
		t.Run(stage, func(t *testing.T) {
			j := job("success")
			j.Result.URL += "?signature=AUDIT_PRIVATE_CANARY"
			c := client(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected API request"); return nil, nil })
			cause := error(errors.New("failed to fetch " + j.Result.URL))
			if stage == "deadline" {
				cause = context.DeadlineExceeded
			}
			if stage == "cancelled" {
				cause = context.Canceled
			}
			if stage == "native" {
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				server.Close()
				j.Result.URL = server.URL + "/audio?signature=AUDIT_PRIVATE_CANARY"
			} else {
				c.downloadClient = isolatedClient(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
					if stage == "body" {
						r := response(200, "")
						r.Body = failedDownloadBody{cause}
						return r, nil
					}
					return nil, cause
				})})
			}
			_, err := c.Download(context.Background(), j)
			var op *OperationError
			if !errors.As(err, &op) || op.RequestID != requestID || op.JobID != jobID {
				t.Fatal("missing recovery identifiers")
			}
			var raw *url.Error
			if errors.As(err, &raw) {
				t.Fatal("private URL is still reachable through errors.As")
			}
			for current := err; current != nil; current = errors.Unwrap(current) {
				for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
					message := fmt.Sprintf(format, current)
					if strings.Contains(message, "AUDIT_PRIVATE_CANARY") || strings.Contains(message, j.Result.URL) {
						t.Fatal("private URL exposed by error formatting")
					}
				}
			}
			if (stage == "deadline" || stage == "cancelled") && !errors.Is(err, cause) {
				t.Fatal("lost context error identity")
			}
		})
	}
}

func TestReflectedErrorMetadataIsSafeToLog(t *testing.T) {
	for _, unsafe := range []string{"fixture-key", "prefix-fixture-key-suffix", "https://audio.example.test/private?signature=CANARY"} {
		c := client(t, func(*http.Request) (*http.Response, error) {
			data, _ := json.Marshal(map[string]any{"error": map[string]any{"code": unsafe, "message": unsafe, "retryable": false}, "traceId": unsafe})
			return response(401, string(data), "X-Request-Id", unsafe), nil
		})
		_, err := c.CreateJob(context.Background(), SynthesisRequest{Text: "Hi", RequestID: requestID})
		var api *APIError
		var op *OperationError
		if !errors.As(err, &api) || !errors.As(err, &op) || op.RequestID != requestID || api.ErrorCode != "api_error" || api.TraceID != "" {
			t.Fatal("unsafe API metadata or missing recovery ID")
		}
		for current := err; current != nil; current = errors.Unwrap(current) {
			if strings.Contains(fmt.Sprintf("%s %v %+v %#v", current, current, current, current), unsafe) {
				t.Fatal("reflected value reached error output")
			}
		}
		j := job("failed")
		j.Error = &JobFailure{Code: unsafe, Message: unsafe}
		_, err = c.WaitJob(context.Background(), j)
		if !errors.As(err, &op) || op.RequestID != requestID || op.JobID != jobID || strings.Contains(err.Error(), unsafe) {
			t.Fatal("unsafe terminal error or missing IDs")
		}
	}
}
func TestRedirectsAndCookies(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("cookie leaked")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(origin.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "fixture"}})
	h := &http.Client{Jar: jar}
	c, err := NewClient("fixture-key", WithBaseURL(origin.URL), WithHTTPClient(h), WithDownloadHTTPClient(h))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Quota(context.Background()); err == nil {
		t.Fatal("API redirect accepted")
	}
	j := job("success")
	j.Result.URL = origin.URL
	if _, err = c.Download(context.Background(), j); err == nil {
		t.Fatal("download redirect accepted")
	}
	if hits != 0 || h.Jar == nil || h.CheckRedirect != nil {
		t.Fatal("followed redirect or mutated caller")
	}
}
func TestConfiguration(t *testing.T) {
	for _, key := range []string{"", " ", "bad\r\nkey", "key value"} {
		if _, err := NewClient(key); err == nil {
			t.Fatal("accepted key")
		}
	}
	for _, raw := range []string{"file:///tmp/x", "http://example.test", "https://user:pass@example.test", "https://example.test/#x", "https://example.test/api"} {
		if _, err := NewClient("fixture-key", WithBaseURL(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	for _, o := range []Option{WithTimeout(0), WithPollInterval(time.Second), WithMaxRetries(-1), WithMaxDownloadBytes(0), WithHTTPClient(nil), WithDownloadHTTPClient(nil), nil} {
		if _, err := NewClient("fixture-key", o); err == nil {
			t.Fatal("accepted option")
		}
	}
	if _, err := (Audio{AudioBase64: "%%%"}).Bytes(); err == nil {
		t.Fatal("invalid base64")
	}
}
