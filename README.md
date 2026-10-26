# FlowSpeech Go

Server-side Go client for the [FlowSpeech public API v1](https://flowspeech.io/docs/public-api).
Requires Go 1.22+, an active paid Basic, Pro or Scale subscription (not a trial),
and an API key from [API settings](https://flowspeech.io/settings/apikeys).

```sh
go get github.com/waeckerlinfederowicz66-sketch/flowspeech-go@v0.2.0
```

Set `FLOWSPEECH_API_KEY` through your environment or secret manager. Never embed
it in browser/mobile applications or commit it. Generation uses account credits.

## Generate WAV Audio

```go
package main

import (
    "context"
    "os"
    "time"
    flowspeech "github.com/waeckerlinfederowicz66-sketch/flowspeech-go"
)

func main() {
    client, err := flowspeech.NewClient(os.Getenv("FLOWSPEECH_API_KEY"))
    if err != nil { panic(err) }
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()
    requestID, err := flowspeech.NewRequestID()
    if err != nil { panic(err) }
    // Persist requestID before submitting; reuse it only with unchanged input.
    audio, err := client.Synthesize(ctx, flowspeech.SynthesisRequest{
        Text: "Welcome to FlowSpeech.", RequestID: requestID,
        Speakers: []flowspeech.Speaker{{VoiceName: "Kore"}},
    })
    if err != nil { panic(err) }
    data, err := audio.Bytes()
    if err != nil { panic(err) }
    if err := os.WriteFile("speech.wav", data, 0o600); err != nil { panic(err) }
}
```

`Synthesize` creates a job, polls to `success`, then downloads WAV audio
(24000 Hz, mono, 16 bit). `AudioBase64` is encoded **locally** for compatibility;
the API does not return base64. `Audio` includes `JobID`, `RequestID`, and `Usage`.
`Audio.Quota` is not populated by v1; query `Quota` separately.

## Explicit Dialogue

```go
audio, err := client.Synthesize(ctx, flowspeech.SynthesisRequest{
    Segments: []flowspeech.Segment{
        {Speaker: "Alice", Text: "Welcome!", VoiceName: "Kore"},
        {Speaker: "Bob", Text: "Thank you.", VoiceName: "Puck"},
    },
})
```

Do not combine `Segments` with `Text` or `Speakers`. Legacy labelled scripts,
multiple `Speakers`, and `OriginalText` are rejected; convert them to explicit
turns. No model, source, speed or output-format option is sent or supported.

## Jobs, Recovery And Account Data

- `CreateJob(ctx, request)` returns the direct v1 job object after HTTP 202/200.
- `GetJob(ctx, jobID)`, `FindJob(ctx, requestID)`, and `CancelJob(ctx, jobID)` expose status, recovery lookup, and cancellation.
- `WaitJob(ctx, job)` waits through queued/running/merging/settling and rejects failed/cancelled/deleted results.
- `Download(ctx, successfulJob)` returns bytes without creating a job. Retry this method after a download failure.
- `Voices(ctx)` reads the top-level voices list.
- `Quota(ctx)` exposes `Remaining`, `Reserved`, `Available`, `Entitlement`, `Subscription`, and `Limits`. Use `Available` for new work. Legacy quota fields remain zero/unset.

The server returns `subscription`; `entitlement` is also accepted for
compatibility. Both are preserved separately. HTTP 200 does not mean generation succeeded.
A deleted recording has `Status == "success"`, `Result == nil`, and
`ResultDeleted == true`; waiting or downloading returns an explicit error.

Use `errors.As(err, &operationError)` with `*flowspeech.OperationError` to recover
request/job IDs, including after a lost create response. `*flowspeech.APIError`
exposes `StatusCode`, `ErrorCode`, `Retryable`, `RetryAfter`, and `TraceID`.
Use `ErrorCode`, not the deprecated numeric `Code` field. `errors.Is` supports
context cancellation/deadline errors.

After an interrupted create, look up the same request UUID or retry `CreateJob`
with the **same UUID and body**. Never use a new UUID merely to retry a download.
Cancelling your local context stops waiting; it does not cancel the server job.

## Limits And Security

Defaults: 10-minute total operation deadline, 30-second HTTP request timeout,
5-second minimum poll/retry interval, two retries for network failures, HTTP 429,
or retryable 5xx errors, 1 MiB API responses, and 64 MiB audio downloads.
Longer numeric or HTTP-date `Retry-After` is honored within the deadline.

Downloads validate the complete RIFF/WAVE container, PCM format (24000 Hz,
mono, 16 bit), byte rate/block alignment, and nonempty complete sample data.
Truncated, malformed or mismatched audio is rejected even on HTTP 200. Bounded,
word-aligned unknown chunks are skipped; valid files are returned unchanged.

Options: `WithTimeout`, `WithPollInterval` (at least 5 seconds), `WithMaxRetries`
(0..10), `WithMaxDownloadBytes`, `WithBaseURL`, `WithHTTPClient`, and
`WithDownloadHTTPClient`. A context deadline can shorten the total deadline.
All retries reuse the serialized body and UUID.

API and audio clients are separate. Their cookie jars are removed and redirects
are refused, including same-origin redirects. Audio requests contain no API
credentials. URLs must use HTTPS without userinfo/fragments; HTTP loopback is
accepted for tests. Injected transports are trusted code and must not add
credentials, cookies or redirect behavior. Result URLs are bearer-like public
links: anyone with a link can download it. Do not log them unnecessarily.

## Development

```sh
go test -race -cover ./...
go vet ./...
go build ./...
```

Tests use fake transports and loopback servers only. See [CHANGELOG.md](CHANGELOG.md)
for the breaking pre-1.0 migration. License: MIT.
