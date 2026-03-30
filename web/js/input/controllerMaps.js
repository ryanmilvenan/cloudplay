/**
 * Per-system controller mappings.
 *
 * Each map translates Standard Gamepad API button indices to KEY constants.
 * See: https://w3c.github.io/gamepad/#remapping
 *
 * Standard Gamepad buttons:
 *   0=A  1=B  2=X  3=Y  4=LB  5=RB  6=LT  7=RT
 *   8=Back/View  9=Start/Menu  10=L3  11=R3
 *   12=Up  13=Down  14=Left  15=Right  16=Guide
 */

import {KEY} from 'input';

// Default: 1:1 Xbox → retro mapping, dpad mode (analog stick → digital)
const defaultMap = {
    dpadMode: true,
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
    }
};

// GameCube (Dolphin) — needs analog sticks
const gcMap = {
    dpadMode: false,
    buttons: {
        0: KEY.A,       // Xbox A → GC A
        1: KEY.B,       // Xbox B → GC B (also acts as GC B)
        2: KEY.X,       // Xbox X → GC X
        3: KEY.Y,       // Xbox Y → GC Y
        4: KEY.L,       // Xbox LB → GC L (digital)
        5: KEY.R,       // Xbox RB → GC R (digital)
        6: KEY.L2,      // Xbox LT → GC L (analog)
        7: KEY.R2,      // Xbox RT → GC R (analog) / GC Z
        8: KEY.SELECT,  // Xbox Back → (unused on GC, but keep)
        9: KEY.START,   // Xbox Menu → GC Start
        10: KEY.L3,
        11: KEY.R3,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    }
};

// N64 (mupen64plus) — needs analog stick, C-buttons on right stick
const n64Map = {
    dpadMode: false,
    buttons: {
        0: KEY.A,       // Xbox A → N64 A
        1: KEY.B,       // Xbox B → N64 B
        2: KEY.X,       // Xbox X → (C-left via button)
        3: KEY.Y,       // Xbox Y → (C-up via button)
        4: KEY.L,       // Xbox LB → N64 L
        5: KEY.R,       // Xbox RB → N64 R
        6: KEY.L2,      // Xbox LT → N64 Z
        7: KEY.R2,      // Xbox RT → N64 Z (alt)
        8: KEY.SELECT,
        9: KEY.START,   // Xbox Menu → N64 Start
        10: KEY.L3,
        11: KEY.R3,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    }
};

// SNES — d-pad only, no analog needed
const snesMap = {
    dpadMode: true,
    buttons: {
        0: KEY.B,       // Xbox A → SNES B (confirm)
        1: KEY.A,       // Xbox B → SNES A (cancel) — swapped for Nintendo layout feel
        2: KEY.Y,       // Xbox X → SNES Y
        3: KEY.X,       // Xbox Y → SNES X
        4: KEY.L,       // Xbox LB → SNES L
        5: KEY.R,       // Xbox RB → SNES R
        8: KEY.SELECT,
        9: KEY.START,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    }
};

// NES — d-pad only, simple 2-button
const nesMap = {
    dpadMode: true,
    buttons: {
        0: KEY.A,       // Xbox A → NES A
        1: KEY.B,       // Xbox B → NES B
        8: KEY.SELECT,
        9: KEY.START,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    }
};

// GBA — d-pad, L/R shoulders
const gbaMap = {
    dpadMode: true,
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
    }
};

// PSX — needs analog sticks
const psxMap = {
    dpadMode: false,
    buttons: {
        0: KEY.A,       // Xbox A → PSX Cross
        1: KEY.B,       // Xbox B → PSX Circle
        2: KEY.Y,       // Xbox X → PSX Square
        3: KEY.X,       // Xbox Y → PSX Triangle
        4: KEY.L,       // Xbox LB → PSX L1
        5: KEY.R,       // Xbox RB → PSX R1
        6: KEY.L2,      // Xbox LT → PSX L2
        7: KEY.R2,      // Xbox RT → PSX R2
        8: KEY.SELECT,
        9: KEY.START,
        10: KEY.L3,
        11: KEY.R3,
        12: KEY.UP,
        13: KEY.DOWN,
        14: KEY.LEFT,
        15: KEY.RIGHT,
    }
};

// System name → map. Names match the `system` field from the game list.
const systemMaps = {
    'GC':   gcMap,
    'gc':   gcMap,
    'Wii':  gcMap,     // Wii uses GC-compatible controls for now
    'N64':  n64Map,
    'n64':  n64Map,
    'SNES': snesMap,
    'snes': snesMap,
    'NES':  nesMap,
    'nes':  nesMap,
    'GBA':  gbaMap,
    'gba':  gbaMap,
    'PSX':  psxMap,
    'psx':  psxMap,
    'MAME': defaultMap,
    'mame': defaultMap,
    'DOS':  defaultMap,
    'dos':  defaultMap,
};

export const getControllerMap = (system) => {
    return systemMaps[system] || defaultMap;
};

export const controllerMaps = {defaultMap, systemMaps, getControllerMap};
