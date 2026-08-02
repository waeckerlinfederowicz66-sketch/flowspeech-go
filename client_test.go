package flowspeech

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSynthesize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ai/text-to-speech" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected authorization: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"mimeType":"audio/wav","audioBase64":"aGVsbG8=","sampleRate":24000,"numChannels":1,"bitsPerSample":16}}`))
	}))
	defer server.Close()

	client, err := NewClient("test-key", WithBaseURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	audio, err := client.Synthesize(context.Background(), SynthesisRequest{Text: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := audio.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("unexpected audio: %q", data)
	}
}

func TestQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"quota":{"limit":10000,"used":120,"remaining":9880,"isGuest":false}}}`))
	}))
	defer server.Close()

	client, _ := NewClient("test-key", WithBaseURL(server.URL))
	quota, err := client.Quota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if quota.Remaining != 9880 {
		t.Fatalf("unexpected remaining quota: %d", quota.Remaining)
	}
}

func TestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":-1,"message":"slow down","data":{"errorCode":"RATE_LIMITED","retryable":true}}`))
	}))
	defer server.Close()

	client, _ := NewClient("test-key", WithBaseURL(server.URL))
	_, err := client.Quota(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if !apiErr.Retryable || apiErr.RetryAfter != "5" {
		t.Fatalf("unexpected APIError: %#v", apiErr)
	}
}

func TestAudioBytesRejectsInvalidBase64(t *testing.T) {
	_, err := (Audio{AudioBase64: "%%%"}).Bytes()
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := err.(base64.CorruptInputError); ok {
		t.Fatal("expected a contextual error")
	}
}
