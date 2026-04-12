// Shared state store for the app lifecycle.
//
// Phase 1 shim for the future state.js (Phase 3). All modules that read
// or mutate cross-cutting app state (current state machine node, last
// state, whether the user has interacted, the shared-session fallback
// timer) import from here. Live bindings keep every reader consistent
// without an event bus hop.

import {log} from 'log';

export const store = {
    state: undefined,
    lastState: undefined,
    interacted: false,
    sharedSessionFallbackTimer: null,
};

let initial;

/** Called once by lifecycle.js after app.state.* is defined. */
export const setInitialState = (s) => { initial = s; };

/**
 * State machine transition.
 * @param newState  A new state strictly from app.state.* (lifecycle.js).
 */
export const setState = (newState) => {
    if (newState === undefined) newState = initial;
    if (newState === store.state) return;

    const prev = store.state;

    if (store.state && store.state._uber) {
        if (store.lastState === newState) store.state = newState;
        store.lastState = newState;
    } else {
        store.lastState = store.state;
        store.state = newState;
    }

    if (log.level === log.DEBUG) {
        const previous = prev ? prev.name : '???';
        const current = store.state ? store.state.name : '???';
        const kept = store.lastState ? store.lastState.name : '???';
        log.debug(`[state] ${previous} -> ${current} [${kept}]`);
    }
};
