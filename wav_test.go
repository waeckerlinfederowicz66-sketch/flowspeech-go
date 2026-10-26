package flowspeech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func wavFormat() []byte {
	f := make([]byte, 16)
	binary.LittleEndian.PutUint16(f[0:], 1)
	binary.LittleEndian.PutUint16(f[2:], 1)
	binary.LittleEndian.PutUint32(f[4:], 24000)
	binary.LittleEndian.PutUint32(f[8:], 48000)
	binary.LittleEndian.PutUint16(f[12:], 2)
	binary.LittleEndian.PutUint16(f[14:], 16)
	return f
}
func wavChunk(tag string, payload []byte) []byte {
	b := make([]byte, 8+len(payload)+(len(payload)&1))
	copy(b, tag)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}
func wavFile(chunks ...[]byte) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WAVE")
	for _, chunk := range chunks {
		b = append(b, chunk...)
	}
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	return b
}
func validWAV() []byte {
	return wavFile(wavChunk("fmt ", wavFormat()), wavChunk("data", []byte{0, 0, 1, 0}))
}
func corruptWAVs() map[string][]byte {
	f, d := wavChunk("fmt ", wavFormat()), wavChunk("data", []byte{0, 0, 1, 0})
	bad := map[string][]byte{
		"empty": nil, "html": []byte("<html>not audio</html>"), "magic only": wavFile(make([]byte, 32)),
		"missing format": wavFile(d), "missing data": wavFile(f), "empty data": wavFile(f, wavChunk("data", nil)),
		"odd data": wavFile(f, wavChunk("data", []byte{1, 2, 3})), "data before format": wavFile(d, f),
		"duplicate format": wavFile(f, f, d), "duplicate data": wavFile(f, d, d),
		"short format":         wavFile(wavChunk("fmt ", wavFormat()[:15]), d),
		"invalid extension":    wavFile(wavChunk("fmt ", append(wavFormat(), 1, 0)), d),
		"partial chunk header": wavFile(f, d, []byte("ABC")),
		"chunk overflow":       wavFile(f, d, []byte{'J', 'U', 'N', 'K', 255, 255, 255, 255}),
		"missing padding":      wavFile(f, d, []byte{'J', 'U', 'N', 'K', 1, 0, 0, 0, 42}),
	}
	for name, change := range map[string]struct {
		offset int
		value  uint32
		width  int
	}{
		"float": {0, 3, 2}, "stereo": {2, 2, 2}, "sample rate": {4, 44100, 4}, "byte rate": {8, 96000, 4}, "block alignment": {12, 4, 2}, "bit depth": {14, 8, 2},
	} {
		format := wavFormat()
		if change.width == 2 {
			binary.LittleEndian.PutUint16(format[change.offset:], uint16(change.value))
		} else {
			binary.LittleEndian.PutUint32(format[change.offset:], change.value)
		}
		bad[name] = wavFile(wavChunk("fmt ", format), d)
	}
	for _, name := range []string{"riff length too big", "riff length too small", "wrong RIFF", "wrong WAVE", "trailing bytes", "data length too big"} {
		b := validWAV()
		switch name {
		case "riff length too big":
			binary.LittleEndian.PutUint32(b[4:], 4096)
		case "riff length too small":
			binary.LittleEndian.PutUint32(b[4:], 4)
		case "wrong RIFF":
			copy(b, "RIFX")
		case "wrong WAVE":
			copy(b[8:], "HTML")
		case "trailing bytes":
			b = append(b, 0)
		case "data length too big":
			binary.LittleEndian.PutUint32(b[40:], 6)
		}
		bad[name] = b
	}
	return bad
}

func TestWAVPreservesValidPCMAndUnknownChunks(t *testing.T) {
	variants := [][]byte{validWAV(), wavFile(wavChunk("JUNK", []byte{1, 2, 3}), wavChunk("fmt ", append(wavFormat(), 0, 0)), wavChunk("LIST", []byte{7}), wavChunk("data", []byte{0, 0, 1, 0}), wavChunk("JUNK", nil))}
	for _, want := range variants {
		c := client(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected API request"); return nil, nil })
		c.downloadClient = isolatedClient(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return response(200, string(want)), nil })})
		got, err := c.Download(context.Background(), job("success"))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("WAV changed: %v", err)
		}
	}
}
func TestWAVRejectsMalformedDownloads(t *testing.T) {
	for name, data := range corruptWAVs() {
		t.Run(name, func(t *testing.T) {
			c := client(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected API request"); return nil, nil })
			c.downloadClient = isolatedClient(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return response(200, string(data)), nil })})
			_, err := c.Download(context.Background(), job("success"))
			var op *OperationError
			if !errors.As(err, &op) || op.JobID != jobID || op.RequestID != requestID || !strings.Contains(err.Error(), "invalid WAV") {
				t.Fatal(err)
			}
		})
	}
}
func TestWAVRejectsEveryTruncatedPrefix(t *testing.T) {
	wav := validWAV()
	for length := 0; length < len(wav); length++ {
		if validateWAV(context.Background(), wav[:length]) == nil {
			t.Fatalf("accepted %d-byte prefix", length)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateWAV(ctx, wav); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestCorruptWAVDoesNotResubmitAndAllowsDownloadRecovery(t *testing.T) {
	calls := 0
	c := client(t, func(*http.Request) (*http.Response, error) { calls++; return jobResponse("success", 200), nil })
	downloads := 0
	c.downloadClient = isolatedClient(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		downloads++
		if downloads == 1 {
			return response(200, "<html>bad</html>"), nil
		}
		return response(200, string(validWAV())), nil
	})})
	_, err := c.Synthesize(context.Background(), SynthesisRequest{Text: "Hi", RequestID: requestID})
	var op *OperationError
	if !errors.As(err, &op) || op.JobID != jobID || op.RequestID != requestID {
		t.Fatal(err)
	}
	if _, err = c.Download(context.Background(), job("success")); err != nil || calls != 1 || downloads != 2 {
		t.Fatal(err, calls, downloads)
	}
}
