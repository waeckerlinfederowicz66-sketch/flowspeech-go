package flowspeech_test

import (
	"context"
	"fmt"
	"os"

	flowspeech "github.com/waeckerlinfederowicz66-sketch/flowspeech-go"
)

func ExampleClient_Synthesize() {
	client, err := flowspeech.NewClient(os.Getenv("FLOWSPEECH_API_KEY"))
	if err != nil {
		panic(err)
	}

	audio, err := client.Synthesize(context.Background(), flowspeech.SynthesisRequest{
		Text:     "Welcome to FlowSpeech.",
		Speakers: []flowspeech.Speaker{{VoiceName: "Kore"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(audio.MIMEType)
}
