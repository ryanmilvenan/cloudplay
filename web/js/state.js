// Centralised app state. Single source of truth for every cross-cutting
// field — current state machine node, slot, roster, identity, input
// readiness. Writers go through setState(patch) — domain-specific
// mutators (e.g. setAppState for state-machine transitions) live with
// their domain module (lifecycle.js), not here. This file knows
// nothing about state-machine shapes.
//
// Keep UI-local state (overlay.isOpen, stream.videoEl.muted) in the
// module that owns it. Only state read or written by two+ modules
// belongs here.

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
