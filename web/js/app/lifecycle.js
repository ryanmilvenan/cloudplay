// App state machine: the state.* objects (eden / settings / menu / game),
// the transitions that move between them (showMenuScreen, the shared-
// session fallback timer, onConnectionReady), and the no-op scaffolding
// (_default, _nil) shared across states.

import {gui} from 'gui';
import {KEY, input} from 'input';
import {log} from 'log';
import {settings} from 'settings';
import {api} from 'api';
import {webrtc} from 'network';

import {gameList} from '../gameList.js?v=__V__';
import {menu} from '../menu.js?v=__V__';
import {message} from '../message.js?v=__V__';
import {overlay} from '../overlay.js?v=__V__';
import {room} from '../room.js?v=__V__';
import {screen} from '../screen.js?v=__V__';
import {stats} from '../stats.js?v=__V__';

import {getState, setState} from 'state';
import {keyButtons, handleToggle} from './keys.js?v=__V__';
import {startGame, saveGame, loadGame, updatePlayerIndex} from './session.js?v=__V__';

let _initialAppState;
export const setInitialAppState = (s) => { _initialAppState = s; };

/**
 * State-machine transition. Handles the _uber flag (settings panel
 * pops back to previous non-uber state), the debug log line, and
 * keeping the derived isGameRunning flag in sync. Writes to the
 * generic store via setState; this is the only sanctioned writer of
 * appState / lastAppState / isGameRunning.
 */
export const setAppState = (newAppState) => {
    if (newAppState === undefined) newAppState = _initialAppState;
    const current = getState();
    if (newAppState === current.appState) return;

    const prev = current.appState;
    let next;
    let last;

    if (prev && prev._uber) {
        next = current.lastAppState === newAppState ? newAppState : prev;
        last = newAppState;
    } else {
        next = newAppState;
        last = prev;
    }

    setState({
        appState: next,
        lastAppState: last,
        isGameRunning: next && next.name === 'game',
    });

    if (log.level === log.DEBUG) {
        const p = prev ? prev.name : '???';
        const c = next ? next.name : '???';
        const k = last ? last.name : '???';
        log.debug(`[state] ${p} -> ${c} [${k}]`);
    }
};

const SHARED_SESSION_FALLBACK_MS = 20000;

// Lifecycle-local — no other module reads or writes this.
let sharedSessionFallbackTimer = null;

export const cancelSharedSessionFallback = () => {
    clearTimeout(sharedSessionFallbackTimer);
    sharedSessionFallbackTimer = null;
};

export const showMenuScreen = () => {
    cancelSharedSessionFallback();
    log.debug('[control] loading menu screen');

    gui.hide(keyButtons[KEY.SAVE]);
    gui.hide(keyButtons[KEY.LOAD]);

    overlay.disable();

    gameList.show();
    screen.toggle(menu);

    setAppState(app.state.menu);
};

export const armSharedSessionFallback = () => {
    cancelSharedSessionFallback();

    if (!room.id) return;

    sharedSessionFallbackTimer = setTimeout(() => {
        if (!room.id || getState().appState === app.state.game || webrtc.isInputReady()) return;

        log.warn(`[control] shared session attach timed out after ${SHARED_SESSION_FALLBACK_MS}ms; falling back to game list`);
        room.reset();
        message.show('Shared session unavailable. Pick a game.');
        showMenuScreen();
    }, SHARED_SESSION_FALLBACK_MS);
};

export const onConnectionReady = () => {
    cancelSharedSessionFallback();
    if (room.id) {
        message.show('Joining current session...');
        startGame();
    } else {
        getState().appState.menuReady();
    }
};

const _nil = () => ({});
const _default = {
    name: 'default',
    axisChanged: _nil,
    keyPress: _nil,
    keyRelease: _nil,
    menuReady: _nil,
};

export const app = {
    state: {
        eden: {
            ..._default,
            name: 'eden',
            menuReady: showMenuScreen,
        },

        settings: {
            ..._default,
            _uber: true,
            name: 'settings',
            keyRelease: (key) => key === KEY.SETTINGS && settings.ui.toggle(),
            menuReady: showMenuScreen,
        },


        menu: {
            ..._default,
            name: 'menu',
            axisChanged: (id, val) => {
                if (id === 1) {
                    gameList.scroll(val < -.5 ? -1 : val > .5 ? 1 : 0);
                }
            },
            keyPress: (key) => {
                switch (key) {
                    case KEY.UP:
                    case KEY.DOWN:
                        gameList.scroll(key === KEY.UP ? -1 : 1);
                        break;
                }
            },
            keyRelease: (key) => {
                switch (key) {
                    case KEY.UP:
                    case KEY.DOWN:
                        gameList.scroll(0);
                        break;
                    case KEY.JOIN:
                    case KEY.A:
                    case KEY.B:
                    case KEY.X:
                    case KEY.Y:
                    case KEY.START:
                    case KEY.SELECT:
                        startGame();
                        break;
                    case KEY.QUIT:
                        message.show('You are already in menu screen!');
                        break;
                    case KEY.LOAD:
                        message.show('Loading the game.');
                        break;
                    case KEY.SAVE:
                        message.show('Saving the game.');
                        break;
                    case KEY.STATS:
                        stats.toggle();
                        break;
                    case KEY.SETTINGS:
                        break;
                    case KEY.DTOGGLE:
                        handleToggle();
                        break;
                }
            },
        },

        game: {
            ..._default,
            name: 'game',
            axisChanged: (id, value) => input.retropad.setAxisChanged(id, value),
            keyboardInput: (pressed, e) => api.game.input.keyboard.press(pressed, e),
            mouseMove: (e) => api.game.input.mouse.move(e.dx, e.dy),
            mousePress: (e) => api.game.input.mouse.press(e.b, e.p),
            keyPress: (key) => {
                if (!overlay.isOpen) {
                    input.retropad.setKeyState(key, true);
                }
            },
            keyRelease: function (key) {
                if (!overlay.isOpen) {
                    input.retropad.setKeyState(key, false);
                }

                switch (key) {
                    case KEY.JOIN: // or SHARE
                        message.show('Use the main site URL to join the shared session');
                        break;
                    case KEY.SAVE:
                        saveGame();
                        break;
                    case KEY.LOAD:
                        loadGame();
                        break;
                    case KEY.FULL:
                        screen.fullscreen();
                        break;
                    case KEY.PAD1:
                        updatePlayerIndex(0);
                        break;
                    case KEY.PAD2:
                        updatePlayerIndex(1);
                        break;
                    case KEY.PAD3:
                        updatePlayerIndex(2);
                        break;
                    case KEY.PAD4:
                        updatePlayerIndex(3);
                        break;
                    case KEY.QUIT:
                        message.show('Killing session...');
                        overlay.disable();
                        input.retropad.toggle(false);
                        api.game.quit(room.id);
                        break;
                    case KEY.RESET:
                        api.game.reset(room.id);
                        break;
                    case KEY.STATS:
                        stats.toggle();
                        break;
                    case KEY.DTOGGLE:
                        handleToggle();
                        break;
                }
            },
        },
    },
};

setInitialAppState(app.state.eden);

// Return to the prior state when the settings panel closes.
settings.ui.onToggle = (open) => { if (!open) setAppState(getState().lastAppState); };
