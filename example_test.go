package flowspeech_test

import (
	"context"
	"fmt"
	"os"
	"time"

	flowspeech "github.com/waeckerlinfederowicz66-sketch/flowspeech-go"
)

func ExampleClient_Synthesize() {
	client, err := flowspeech.NewClient(os.Getenv("FLOWSPEECH_API_KEY"))
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	requestID, err := flowspeech.NewRequestID()
	if err != nil {
		panic(err)
	}
	fmt.Println("Persist this request ID for recovery:", requestID)
	audio, err := client.Synthesize(ctx, flowspeech.SynthesisRequest{
		Text:      "Welcome to FlowSpeech.",
		RequestID: requestID,
		Speakers:  []flowspeech.Speaker{{VoiceName: "Kore"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(audio.MIMEType)
}
