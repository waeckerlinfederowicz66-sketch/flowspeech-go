# Changelog

## 0.2.0

- Validate RIFF/WAVE chunk sizes, 24000 Hz mono 16-bit PCM format and complete aligned nonempty audio data; preserve valid files byte-for-byte and reject malformed downloads without resubmitting.

- Migrated to public API v1 asynchronous jobs, stable UUID retries, recovery lookup, cancellation, voices and available-credit quota.
- Retained single-voice `Synthesize`/`Audio.Bytes`; audio is downloaded as WAV and base64-encoded locally.
- Added explicit multi-speaker segments; legacy labelled dialogue and original text are rejected.
- Added total deadlines, Retry-After-aware polling, terminal/deleted-result validation, bounded downloads, redirect refusal and credential isolation.
- Added offline transport and loopback security tests. Requires an active paid API subscription and key.

## 0.1.0

- Initial synchronous API client. Superseded by 0.2.0.
