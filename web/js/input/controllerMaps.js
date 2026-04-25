/**
 * Per-system controller mappings.
 *
 * Each map translates Standard Gamepad API button indices to KEY constants
 * and declares whether the platform expects analog sticks / triggers.
 * See: https://w3c.github.io/gamepad/#remapping
 *
 * Standard Gamepad buttons:
 *   0=A  1=B  2=X  3=Y  4=LB  5=RB  6=LT  7=RT
 *   8=Back/View  9=Start/Menu  10=L3  11=R3
 *   12=Up  13=Down  14=Left  15=Right  16=Guide
 */

import {KEY} from 'input';

const makeMap = ({analogAxes = false, analogTriggers = false, buttons}) => ({
    analogAxes,
    analogTriggers,
    buttons,
});

// Default: 1:1 Xbox → retro mapping, keep analog sticks/triggers enabled.
const defaultMap = makeMap({
    analogAxes: true,
    analogTriggers: true,
    buttons: {
        0: KEY.A,
        1: KEY.B,
        2: KEY.X,
        3: KEY.Y,
        4: KEY.L,
        5: KEY.R,
        6: KEY.L2,
        7: KEY.R2,
        8: KEY.SELECT,
        9: KEY.START,
        10: KEY.L3,
        11: KEY.R3,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    },
});

// GameCube (Dolphin) — needs analog sticks + analog triggers.
const gcMap = makeMap({
    analogAxes: true,
    analogTriggers: true,
    buttons: {
        0: KEY.A,       // Xbox A → GC A
        1: KEY.B,       // Xbox B → GC B
        2: KEY.X,       // Xbox X → GC X
        3: KEY.Y,       // Xbox Y → GC Y
        4: KEY.L,       // Xbox LB → GC L (digital)
        5: KEY.R,       // Xbox RB → GC R (digital)
        6: KEY.L2,      // Xbox LT → GC L (analog)
        7: KEY.R2,      // Xbox RT → GC R (analog) / GC Z
        8: KEY.SELECT,
        9: KEY.START,   // Xbox Menu → GC Start
        10: KEY.L3,
        11: KEY.R3,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    },
});

// N64 — needs analog stick, but triggers are digital Z-button mappings.
// Face buttons swapped vs. Xbox layout so N64 B sits on the bottom face
// button (Xbox A / PS Cross) instead of the top, matching N64-controller
// muscle memory where B is the primary action button.
const n64Map = makeMap({
    analogAxes: true,
    analogTriggers: false,
    buttons: {
        0: KEY.Y,       // Xbox A / PS Cross → N64 B
        1: KEY.B,
        2: KEY.X,
        3: KEY.A,       // Xbox Y / PS Triangle → (was N64 B)
        4: KEY.L,
        5: KEY.R,
        6: KEY.L2,
        7: KEY.R2,
        8: KEY.SELECT,
        9: KEY.START,
        10: KEY.L3,
        11: KEY.R3,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    },
});

// SNES — d-pad only.
const snesMap = makeMap({
    buttons: {
        0: KEY.B,
        1: KEY.A,
        2: KEY.Y,
        3: KEY.X,
        4: KEY.L,
        5: KEY.R,
        8: KEY.SELECT,
        9: KEY.START,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    },
});

// NES — d-pad only, simple 2-button.
const nesMap = makeMap({
    buttons: {
        0: KEY.A,
        1: KEY.B,
        8: KEY.SELECT,
        9: KEY.START,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    },
});

// GBA — d-pad, L/R shoulders.
const gbaMap = makeMap({
    buttons: {
        0: KEY.A,
        1: KEY.B,
        4: KEY.L,
        5: KEY.R,
        8: KEY.SELECT,
        9: KEY.START,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    },
});

// PSX / PS2 — DualShock-style analog sticks + analog trigger values.
const psxMap = makeMap({
    analogAxes: true,
    analogTriggers: true,
    buttons: {
        // Libretro Retropad uses SNES-style semantic face buttons:
        // bottom=B, right=A, left=Y, top=X.
        // For PlayStation this means Cross->B, Circle->A, Square->Y, Triangle->X.
        0: KEY.B,
        1: KEY.A,
        2: KEY.Y,
        3: KEY.X,
        4: KEY.L,
        5: KEY.R,
        6: KEY.L2,
        7: KEY.R2,
        8: KEY.SELECT,
        9: KEY.START,
        10: KEY.L3,
        11: KEY.R3,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    },
});

// Dreamcast — analog sticks + analog triggers.
const dcMap = makeMap({
    analogAxes: true,
    analogTriggers: true,
    buttons: {...psxMap.buttons},
});

// Xbox (xemu native) — needs the same SNES-convention face-button swap as
// psxMap/dcMap. The wire format from this client to the worker is the
// libretro RetroPad bitmask (B=south, A=east, Y=west, X=north). For Xbox
// we route through pkg/worker/caged/nativeemu/virtualpad.go which translates
// libretro bits → xpad evdev codes; SDL2's built-in 360 mapping in xemu then
// renames those back to A/B/X/Y. Without this swap the user's physical Y
// (top) becomes libretro Y (west) → BTN_X → xemu reports X — exactly the
// "press Y, get X" symptom observed in prod.
const xboxMap = makeMap({
    analogAxes: true,
    analogTriggers: true,
    buttons: {...psxMap.buttons},
});

// System name → map. Names match the `system` field from the game list.
const systemMaps = {
    'GC': gcMap,
    'gc': gcMap,
    'Wii': gcMap,
    'N64': n64Map,
    'n64': n64Map,
    'SNES': snesMap,
    'snes': snesMap,
    'NES': nesMap,
    'nes': nesMap,
    'GBA': gbaMap,
    'gba': gbaMap,
    'PSX': psxMap,
    'psx': psxMap,
    'PS2': psxMap,
    'ps2': psxMap,
    'DC': dcMap,
    'dc': dcMap,
    'Xbox': xboxMap,
    'xbox': xboxMap,
    'MAME': defaultMap,
    'mame': defaultMap,
    'DOS': defaultMap,
    'dos': defaultMap,
};

export const getControllerMap = (system) => systemMaps[system] || defaultMap;

export const controllerMaps = {defaultMap, systemMaps, getControllerMap};
