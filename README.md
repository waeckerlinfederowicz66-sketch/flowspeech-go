# FlowSpeech Go

[![Go Reference](https://pkg.go.dev/badge/github.com/waeckerlinfederowicz66-sketch/flowspeech-go.svg)](https://pkg.go.dev/github.com/waeckerlinfederowicz66-sketch/flowspeech-go)
[![Test](https://github.com/waeckerlinfederowicz66-sketch/flowspeech-go/actions/workflows/test.yml/badge.svg)](https://github.com/waeckerlinfederowicz66-sketch/flowspeech-go/actions/workflows/test.yml)

An idiomatic Go client for the [FlowSpeech text to speech API](https://flowspeech.io/). Generate context-aware speech with expressive direction, multiple speakers, and audio format metadata.

## Install

```bash
go get github.com/waeckerlinfederowicz66-sketch/flowspeech-go
```

Create an API key in your FlowSpeech account, then keep it outside source control:

```bash
export FLOWSPEECH_API_KEY="your_api_key"
```

## Generate speech

```go
package main

import (
    "context"
    "os"

    flowspeech "github.com/waeckerlinfederowicz66-sketch/flowspeech-go"
)

func main() {
    client, err := flowspeech.NewClient(os.Getenv("FLOWSPEECH_API_KEY"))
    if err != nil {
        panic(err)
    }

    audio, err := client.Synthesize(context.Background(), flowspeech.SynthesisRequest{
        Text:     "Welcome to FlowSpeech. [cheerfully] Let's make this sound natural.",
        Speakers: []flowspeech.Speaker{{VoiceName: "Kore"}},
    })
    if err != nil {
        panic(err)
    }

    data, err := audio.Bytes()
    if err != nil {
        panic(err)
    }
    if err := os.WriteFile("speech.wav", data, 0o600); err != nil {
        panic(err)
    }
}
```

## Multi-speaker dialogue

```go
audio, err := client.Synthesize(ctx, flowspeech.SynthesisRequest{
    Text: "Host: Welcome to the show.\nGuest: Thanks for having me.",
    Speakers: []flowspeech.Speaker{
        {Speaker: "Host", VoiceName: "Charon"},
        {Speaker: "Guest", VoiceName: "Kore"},
    },
})
```

## Check quota

```go
quota, err := client.Quota(ctx)
if err != nil {
    panic(err)
}
fmt.Printf("%d characters remaining\n", quota.Remaining)
```

## Error handling

HTTP failures are returned as `*flowspeech.APIError`, including the HTTP status, API error code, retryability, and `Retry-After` value when available.

```go
var apiErr *flowspeech.APIError
if errors.As(err, &apiErr) && apiErr.Retryable {
    // Apply retry/backoff appropriate for your application.
}
```

## Security

- Never commit API keys.
- Load `FLOWSPEECH_API_KEY` from your environment or secret manager.
- The client sends the key only in the `Authorization: Bearer` header.
- Tests use local mock servers and contain no credentials.

## License

MIT
