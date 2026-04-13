/**
 * Event publish / subscribe module — a tiny observer pattern.
 *
 * Contract: each event below has a documented payload shape, publisher,
 * and subscriber. Grep for an event name here first — this file is the
 * source of truth for the pub/sub protocol. Updating a payload shape
 * without updating this file is a bug.
 *
 * Notation used in the JSDocs below:
 *   pub: <where pub() is called>
 *   sub: <where sub() is called>
 *   payload: <shape>   — if omitted, no payload (pub() called with no data)
 *
 * "wiring" refers to app/wiring.js, the single module that owns
 * app-level sub() calls. "endpoint" refers to the server PT codes
 * dispatched inside wiring.onMessage.
 */

const topics = {};
let _index = 0;

/**
 * Subscribe to an event.
 * @param topic    Event name (one of the constants below).
 * @param listener Callback invoked with the event payload.
 * @param order    Lower runs first. Optional; default is arrival order.
 * @returns {{unsub: () => void}}
 */
export const sub = (topic, listener, order = undefined) => {
    if (!topics[topic]) topics[topic] = {};
    // order * big pad + arrival index so ordered handlers sort before
    // unordered ones even after N arrivals.
    let i = (order !== undefined ? order * 1000000 : 0) + _index++;
    topics[topic][i] = listener;
    return {unsub: () => { delete topics[topic][i]; }};
};

/**
 * Publish an event. Handlers receive the data object (or `{}` if none).
 * @param topic Event name.
 * @param data  Payload. Shape per the event's JSDoc below.
 */
export const pub = (topic, data) => {
    if (!topics[topic]) return;
    Object.keys(topics[topic]).forEach((ls) => {
        topics[topic][ls](data !== undefined ? data : {});
    });
};

// ── Worker/server round-trips ───────────────────────────────────────

/** payload: `{packetId, addresses}`. pub: wiring ← LATENCY_CHECK. sub: wiring → onLatencyCheck (session). */
export const LATENCY_CHECK_REQUESTED = 'latencyCheckRequested';

/** payload: server-supplied worker list. pub: wiring ← GET_WORKER_LIST. sub: workerManager.onNewData. */
export const WORKER_LIST_FETCHED = 'workerListFetched';

// ── Game lifecycle ──────────────────────────────────────────────────

/** payload: `{roomId}`. pub: wiring ← GAME_START. sub: wiring (stream.play, order 2) + room.js (persists room.id). */
export const GAME_ROOM_AVAILABLE = 'gameRoomAvailable';

/** no payload. pub: wiring ← GAME_SAVE. sub: wiring (shows "Saved" toast). */
export const GAME_SAVED = 'gameSaved';

/** payload: `{index: number}` (0-based). pub: input/touch.js (drag picker). sub: wiring → updatePlayerIndex. */
export const GAME_PLAYER_IDX = 'gamePlayerIndex';

/** payload: server-assigned slot index. pub: wiring ← GAME_SET_PLAYER_INDEX. sub: wiring (toast shows idx+1). */
export const GAME_PLAYER_IDX_SET = 'gamePlayerIndexSet';

/** no payload. pub: wiring ← GAME_ERROR_NO_FREE_SLOTS. sub: wiring (shows "No free slots"). */
export const GAME_ERROR_NO_FREE_SLOTS = 'gameNoFreeSlots';

// Note: the worker's ROOM_MEMBERS roster snapshot is not an event —
// wiring.onMessage writes it directly to state.js (setState({roomMembers}))
// and overlay re-renders via subscribe.

// ── WebRTC signalling and lifecycle ─────────────────────────────────

/** no payload. pub: webrtc.js (disconnect grace expired). sub: wiring (resetToMenu or retropad teardown). */
export const WEBRTC_CONNECTION_CLOSED = 'webrtcConnectionClosed';

/** no payload. pub: webrtc.js (data channel open). sub: wiring → onConnectionReady (lifecycle). */
export const WEBRTC_CONNECTION_READY = 'webrtcConnectionReady';

/** payload: `{candidate}`. pub: webrtc.js (onicecandidate). sub: wiring → api.server.sendIceCandidate. */
export const WEBRTC_ICE_CANDIDATE_FOUND = 'webrtcIceCandidateFound';

/** payload: `{candidate}`. pub: wiring ← ICE_CANDIDATE. sub: wiring → webrtc.addCandidate. */
export const WEBRTC_ICE_CANDIDATE_RECEIVED = 'webrtcIceCandidateReceived';

/** no payload. pub: webrtc.js (after answer sent). sub: wiring → webrtc.flushCandidates. */
export const WEBRTC_ICE_CANDIDATES_FLUSH = 'webrtcIceCandidatesFlush';

/** payload: `{wid, ice, roomId?, games}`. pub: wiring ← INIT. sub: wiring (wires onData, starts webrtc, loads game list). */
export const WEBRTC_NEW_CONNECTION = 'webrtcNewConnection';

/** payload: `{sdp}`. pub: webrtc.js (after setRemoteDescription offer). sub: wiring → api.server.sendSdp. */
export const WEBRTC_SDP_ANSWER = 'webrtcSdpAnswer';

/** payload: `{sdp}`. pub: wiring ← OFFER. sub: wiring → webrtc.setRemoteDescription. */
export const WEBRTC_SDP_OFFER = 'webrtcSdpOffer';

// ── Transport ───────────────────────────────────────────────────────

/** payload: decoded api message `{id, t, p}`. pub: socket.js (onmessage). sub: wiring → onMessage. */
export const MESSAGE = 'message';

// ── Gamepad connection ──────────────────────────────────────────────

/** no payload. pub: joystick.js. sub: wiring ("Gamepad connected" toast). */
export const GAMEPAD_CONNECTED = 'gamepadConnected';

/** no payload. pub: joystick.js. sub: wiring ("Gamepad disconnected" toast). */
export const GAMEPAD_DISCONNECTED = 'gamepadDisconnected';

// ── Touch menu ──────────────────────────────────────────────────────

/** payload: `{event: string, handler: fn}`. pub: touch.js (init). sub: menu.js (re-registers after re-render). */
export const MENU_HANDLER_ATTACHED = 'menuHandlerAttached';

// ── Keyboard + key dispatch ─────────────────────────────────────────

/** payload: `{key, code?}`. pub: joystick/keyboard/touch. sub: wiring → onKeyPress (keys.js). */
export const KEY_PRESSED = 'keyPressed';

/** payload: `{key, code?}`. pub: joystick/keyboard/touch. sub: wiring → onKeyRelease (keys.js). */
export const KEY_RELEASED = 'keyReleased';

/** payload: `{mode?: boolean}`. pub: recording.js (focus/blur on username input). sub: keyboard.js (suspend game keybinds). */
export const KEYBOARD_TOGGLE_FILTER_MODE = 'keyboardToggleFilterMode';

/** payload: `{key: code}`. pub: keyboard.js (filter mode). sub: settings.js (key rebinding UI). */
export const KEYBOARD_KEY_PRESSED = 'keyboardKeyPressed';

/** payload: raw KeyboardEvent. pub: keyboard.js (in pointer-lock / kbm mode). sub: wiring → state.keyboardInput(true). */
export const KEYBOARD_KEY_DOWN = 'keyboardKeyDown';

/** payload: raw KeyboardEvent. pub: keyboard.js (in pointer-lock / kbm mode). sub: wiring → state.keyboardInput(false). */
export const KEYBOARD_KEY_UP = 'keyboardKeyUp';

// ── Analog input ────────────────────────────────────────────────────

/** payload: `{id, value}`. pub: joystick/keyboard/touch. sub: wiring → onAxisChanged. */
export const AXIS_CHANGED = 'axisChanged';

/** payload: `{id, value}`. pub: joystick.js. sub: wiring → onTriggerChanged (→ input.retropad). */
export const TRIGGER_CHANGED = 'triggerChanged';

/** payload: retropad packet (buttons/axes/triggers). pub: retropad.js. sub: wiring → webrtc.input. */
export const CONTROLLER_UPDATED = 'controllerUpdated';

// ── Mouse (pointer-lock mode) ───────────────────────────────────────

/** payload: `{dx, dy}`. pub: pointer.js. sub: wiring → state.mouseMove. */
export const MOUSE_MOVED = 'mouseMoved';

/** payload: `{b, p}`. pub: pointer.js. sub: wiring → state.mousePress. */
export const MOUSE_PRESSED = 'mousePressed';

// ── Display ─────────────────────────────────────────────────────────

/** no payload. pub: env.js (gameBoy style MutationObserver). sub: stream.js (resize/reposition). */
export const TRANSFORM_CHANGE = 'tc';

// ── UI toggles ──────────────────────────────────────────────────────

/** payload: `{checked: boolean}`. pub: keys.handleToggle / touch.js / keyboard.js. sub: keyboard.js + touch.js (onDpadToggle). */
export const DPAD_TOGGLE = 'dpadToggle';

/** payload: `{shown: boolean}`. pub: keys.helpScreen.show. sub: stats.js (onHelpOverlayToggle). */
export const HELP_OVERLAY_TOGGLED = 'helpOverlayToggled';

// ── Settings ────────────────────────────────────────────────────────

/** no payload. pub: settings.js (on save). sub: screen + stream + settings (render) + wiring (log level + toast). */
export const SETTINGS_CHANGED = 'settingsChanged';

// ── Recording ───────────────────────────────────────────────────────

/** payload: `{userName, recording: boolean}`. pub: recording.js (button click). sub: wiring → api.game.toggleRecording. */
export const RECORDING_TOGGLED = 'recordingToggle';

/** payload: 'ok' | 'fail'. pub: wiring ← GAME_RECORDING. sub: wiring (updates indicator + toast). */
export const RECORDING_STATUS_CHANGED = 'recordingStatusChanged';

// ── App-level stream/input modes ────────────────────────────────────

/** payload: `{s?: scale, w, h}`. pub: wiring ← GAME_START (av) / APP_VIDEO_CHANGE. sub: stream (resize) + statsProbes. */
export const APP_VIDEO_CHANGED = 'appVideoChanged';

/** no payload. pub: wiring ← GAME_START (kb_mouse flag). sub: input.js (enables kbm path) + wiring (toast). */
export const KB_MOUSE_FLAG = 'kbMouseFlag';

/** no payload. pub: wiring.kbmCb + input.js. sub: screen.js (recomputes layout). */
export const REFRESH_INPUT = 'refreshInput';
