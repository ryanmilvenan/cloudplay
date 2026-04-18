# uinput-canary

Deterministic validator for the xemu **uinput virtual-gamepad** input-injection
path used by the native-emulator backend (Phase 5).

## What it does

1. Use `pkg/worker/caged/xemu/input.go` to create a virtual Xbox controller via
   `/dev/uinput`.
2. Spawn `evtest` (pre-installed in the dev container) on the new evdev node.
3. Replay a scripted button-press / stick-movement sequence covering all 14
   Xbox controls (A, B, X, Y, LS, RS, LT, RT, Back, Start, D-pad × 4, sticks × 2).
4. Capture evtest's event log; diff against `testdata/golden_events.txt`.
5. Exit 0 iff diff is empty.

## Why standalone

uinput permissions + udev timing + SDL2 device detection all have their own
failure modes. Decoupling them from xemu makes Phase-5 bugs easy to bisect.

## Running (on moon, needs /dev/uinput with write permission)

```bash
scripts/dev-sync.sh harness uinput-canary
```

## Status

**Phase 0**: scaffold only.
**Phase 5**: full implementation.
