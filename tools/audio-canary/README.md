# audio-canary

Deterministic validator for the xemu **PipeWire audio-capture** path used by
the native-emulator backend (Phase 4).

## What it does

1. Emit a precise 440 Hz sine wave to the default PipeWire sink.
2. Use `pkg/worker/caged/xemu/audiocap.go` to capture ~5 seconds of audio
   (48 kHz S16LE stereo — matches the Opus encoder's expected rate).
3. FFT the captured samples.
4. Assert the peak frequency bin is 440 Hz ± 2 Hz and its magnitude is within
   -3 dB of expected.
5. Exit 0 iff all assertions hold.

## Why standalone

PipeWire capture issues (permission, targeting, format mismatches) are easy to
confuse with xemu bugs. This harness pins down the capture primitive alone so
Phase-4 failures are immediately attributable.

## Running (on moon)

```bash
scripts/dev-sync.sh harness audio-canary
```

## Status

**Phase 0**: scaffold only.
**Phase 4**: full implementation.
