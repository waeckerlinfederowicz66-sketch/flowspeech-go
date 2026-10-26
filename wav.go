package flowspeech

import (
	"context"
	"encoding/binary"
	"errors"
)

// Validate the bounded RIFF container without rewriting or copying its samples.
func validateWAV(ctx context.Context, data []byte) error {
	invalid := func() error {
		return errors.New("invalid WAV: expected complete 24000 Hz mono 16-bit PCM with nonempty aligned data")
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return invalid()
	}
	if uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return invalid()
	}
	fmtSeen, dataSeen := false, false
	for offset := 12; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(data)-offset < 8 {
			return invalid()
		}
		size := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := uint64(start) + size
		next := end + (size & 1)
		if next > uint64(len(data)) {
			return invalid()
		}
		switch string(data[offset : offset+4]) {
		case "fmt ":
			if fmtSeen || dataSeen || (size != 16 && size != 18) {
				return invalid()
			}
			f := data[start:int(end)]
			if binary.LittleEndian.Uint16(f[0:2]) != 1 || binary.LittleEndian.Uint16(f[2:4]) != 1 ||
				binary.LittleEndian.Uint32(f[4:8]) != 24000 || binary.LittleEndian.Uint32(f[8:12]) != 48000 ||
				binary.LittleEndian.Uint16(f[12:14]) != 2 || binary.LittleEndian.Uint16(f[14:16]) != 16 ||
				(size == 18 && binary.LittleEndian.Uint16(f[16:18]) != 0) {
				return invalid()
			}
			fmtSeen = true
		case "data":
			if !fmtSeen || dataSeen || size == 0 || size%2 != 0 {
				return invalid()
			}
			dataSeen = true
		}
		offset = int(next)
	}
	if !fmtSeen || !dataSeen {
		return invalid()
	}
	return ctx.Err()
}
