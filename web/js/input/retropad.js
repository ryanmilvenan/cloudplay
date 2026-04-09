import {pub, CONTROLLER_UPDATED} from 'event';
import {JOYPAD_KEYS} from 'input';

/*
 * [BUTTONS, LEFT_X, LEFT_Y, RIGHT_X, RIGHT_Y, L2, R2]
 *
 * Buttons are packed into a 16-bit bitmask where each bit is one button.
 * Axes are signed 16-bit values ranging from -32768 to 32767.
 * Triggers are signed 16-bit values ranging from 0 to 32767.
 * The whole thing is 14 bytes when sent over the wire.
 */
const state = new Int16Array(7);
let buttons = 0;
let dirty = false;
let rafId = 0;

/*
 * Polls controller state using requestAnimationFrame which gives us
 * ~60Hz update rate that syncs with the display. As a bonus,
 * it automatically pauses when the tab goes to background.
 * We only send data when something actually changed.
 */
let _pollDiagN = 0;
const poll = () => {
    if (dirty) {
        state[0] = buttons;
        const payload = new Uint16Array(state.buffer);
        _pollDiagN++;
        if (_pollDiagN <= 20 || _pollDiagN % 120 === 0) {
            console.log('[retropad] send frame=' + _pollDiagN +
                ' btns=0x' + buttons.toString(16) +
                ' axes=[' + state[1] + ',' + state[2] + ',' + state[3] + ',' + state[4] + ']' +
                ' trig=[' + state[5] + ',' + state[6] + ']');
        }
        pub(CONTROLLER_UPDATED, payload);
        dirty = false;
    }
    rafId = requestAnimationFrame(poll);
};

/*
 * Toggles a button on or off in the bitmask. The button's position
 * in JOYPAD_KEYS determines which bit gets flipped. For example,
 * if A is at index 8, pressing it sets bit 8.
 */
const setKeyState = (key, pressed) => {
    const idx = JOYPAD_KEYS.indexOf(key);
    if (idx < 0) return;

    const prev = buttons;
    buttons = pressed ? buttons | (1 << idx) : buttons & ~(1 << idx);
    dirty ||= buttons !== prev;
};

/*
 * Updates an analog stick axis. Axes 0-1 are the left stick (X and Y),
 * axes 2-3 are the right stick. Input should be a float from -1 to 1
 * which gets converted to a signed 16-bit integer for transmission.
 */
const setAxisChanged = (axis, value) => {
    if (axis < 0 || axis > 3) return;

    const slot = axis + 1;
    const v = Math.trunc(Math.max(-1, Math.min(1, value)) * 32767);
    dirty ||= state[slot] !== v;
    state[slot] = v;
};

/*
 * Updates an analog trigger. triggerIdx 0 = L2, 1 = R2.
 * Input should be a float from 0 to 1 which gets converted
 * to a signed 16-bit integer (0 to 32767) for transmission.
 */
const setTriggerChanged = (triggerIdx, value) => {
    if (triggerIdx < 0 || triggerIdx > 1) return;
    const slot = 5 + triggerIdx; // slots 5 and 6 in the Int16Array
    const v = Math.trunc(Math.max(0, Math.min(1, value)) * 32767);
    dirty ||= state[slot] !== v;
    state[slot] = v;
};

const reset = () => {
    buttons = 0;
    state.fill(0);
    dirty = true;
};

// Starts or stops the polling loop
const toggle = (on) => {
    if (on === !!rafId) return;
    rafId = on ? requestAnimationFrame(poll) : (cancelAnimationFrame(rafId), 0);
};

export const retropad = {toggle, reset, setKeyState, setAxisChanged, setTriggerChanged};
