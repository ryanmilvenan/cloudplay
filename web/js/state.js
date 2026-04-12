// Centralised app state. Single source of truth for every cross-cutting
// field — current state machine node, slot, roster, identity, input
// readiness. Writers go through setState(patch) or the named mutators
// below; readers call getState() or subscribe(fn) for change
// notifications.
//
// Keep UI-local state (overlay.isOpen, stream.videoEl.muted) in the
// module that owns it. Only state read or written by two+ modules
// belongs here.

import {log} from 'log';

const state = {
    /** @type {object | undefined} Current state machine node (from app.state.*). */
    appState: undefined,
    /** @type {object | undefined} Previous state; used by settings _uber to pop back. */
    lastAppState: undefined,
    /** @type {boolean} True once the user has interacted (first key or audio unmute). */
    interacted: false,
    /** @type {object | null} pocket-id identity from oauth2-proxy headers. */
    identity: null,
    /** @type {number} 0-based slot index this client owns. */
    currentSlot: 0,
    /** @type {Array<{user_id, slot, identity}>} Full roster snapshot from the worker. */
    roomMembers: [],
    /** @type {boolean} Derived — state.appState === app.state.game. Kept explicit for subscribers that don't want to import lifecycle. */
    isGameRunning: false,
    /** @type {boolean} True when this client arrived with an existing room.id in the URL. */
    joiningSharedSession: false,
    /** @type {boolean} True once the worker reports the WebRTC data channel is ready. */
    inputReady: false,
};

const listeners = new Set();
const notify = () => { for (const fn of listeners) fn(state); };

/** Read-only view of the current state. Do not mutate directly. */
export const getState = () => state;

/**
 * Merge a partial update into the store and notify subscribers.
 * @param patch Partial<state>.
 */
export const setState = (patch) => {
    Object.assign(state, patch);
    notify();
};

/**
 * Register a change listener. Fires on every setState / mutator call,
 * with the current state as the argument. Returns an unsubscribe fn.
 */
export const subscribe = (fn) => {
    listeners.add(fn);
    return () => listeners.delete(fn);
};

let _initialAppState;
export const setInitialAppState = (s) => { _initialAppState = s; };

/**
 * State-machine transition. Handles the _uber flag (settings panel
 * pops back to previous non-uber state) and the debug log line.
 * This is the only writer for `appState` and `lastAppState`.
 */
export const setAppState = (newAppState) => {
    if (newAppState === undefined) newAppState = _initialAppState;
    if (newAppState === state.appState) return;

    const prev = state.appState;

    if (state.appState && state.appState._uber) {
        if (state.lastAppState === newAppState) state.appState = newAppState;
        state.lastAppState = newAppState;
    } else {
        state.lastAppState = state.appState;
        state.appState = newAppState;
    }

    // Keep the derived isGameRunning flag in sync so subscribers that
    // don't want to import lifecycle (and thus compare against
    // app.state.game) can still react to game-vs-menu transitions.
    state.isGameRunning = state.appState && state.appState.name === 'game';

    if (log.level === log.DEBUG) {
        const previous = prev ? prev.name : '???';
        const current = state.appState ? state.appState.name : '???';
        const kept = state.lastAppState ? state.lastAppState.name : '???';
        log.debug(`[state] ${previous} -> ${current} [${kept}]`);
    }

    notify();
};
