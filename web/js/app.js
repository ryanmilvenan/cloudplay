import {log} from 'log';
import {opts, settings} from 'settings';
import {api} from 'api';
import {
    APP_VIDEO_CHANGED,
    AXIS_CHANGED,
    TRIGGER_CHANGED,
    CONTROLLER_UPDATED,
    DPAD_TOGGLE,
    FULLSCREEN_CHANGE,
    GAME_ERROR_NO_FREE_SLOTS,
    GAME_PLAYER_IDX,
    GAME_PLAYER_IDX_SET,
    GAME_ROOM_AVAILABLE,
    GAME_SAVED,
    GAMEPAD_CONNECTED,
    GAMEPAD_DISCONNECTED,
    HELP_OVERLAY_TOGGLED,
    KB_MOUSE_FLAG,
    KEY_PRESSED,
    KEY_RELEASED,
    KEYBOARD_KEY_DOWN,
    KEYBOARD_KEY_UP,
    LATENCY_CHECK_REQUESTED,
    MESSAGE,
    MOUSE_MOVED,
    MOUSE_PRESSED,
    POINTER_LOCK_CHANGE,
    RECORDING_STATUS_CHANGED,
    RECORDING_TOGGLED,
    REFRESH_INPUT,
    SETTINGS_CHANGED,
    WEBRTC_CONNECTION_CLOSED,
    WEBRTC_CONNECTION_READY,
    WEBRTC_ICE_CANDIDATE_FOUND,
    WEBRTC_ICE_CANDIDATE_RECEIVED,
    WEBRTC_ICE_CANDIDATES_FLUSH,
    WEBRTC_NEW_CONNECTION,
    WEBRTC_SDP_ANSWER,
    WEBRTC_SDP_OFFER,
    WORKER_LIST_FETCHED,
    pub,
    sub,
} from 'event';
import {gui} from 'gui';
import {input, KEY} from 'input';
import {socket, webrtc} from 'network';
import {debounce} from 'utils';

import {gameList} from './gameList.js?v=5';
import {gameListNew} from './gameListNew.js?v=5';
import {menu} from './menu.js?v=5';
import {message} from './message.js?v=5';
import {overlay} from './overlay.js?v=5';
import {recording} from './recording.js?v=5';
import {room} from './room.js?v=5';
import {screen} from './screen.js?v=5';
import {stats} from './stats.js?v=5';
import {stream} from './stream.js?v=5';
import {workerManager} from "./workerManager.js?v=5";

settings.init();
log.level = settings.loadOr(opts.LOG_LEVEL, log.DEFAULT);

// application display state
let state;
let lastState;

// first user interaction
let interacted = false;
let sharedSessionFallbackTimer;

const SHARED_SESSION_FALLBACK_MS = 20000;

const helpOverlay = document.getElementById('help-overlay');
const playerIndex = document.getElementById('playeridx');

// screen init
screen.add(menu, stream);

// keymap
const keyButtons = {};
Object.keys(KEY).forEach(button => {
    keyButtons[KEY[button]] = document.getElementById(`btn-${KEY[button]}`);
});

/**
 * State machine transition.
 * @param newState A new state strictly from app.state.*
 */
const setState = (newState = app.state.eden) => {
    if (newState === state) return;

    const prevState = state;

    if (state && state._uber) {
        if (lastState === newState) state = newState;
        lastState = newState;
    } else {
        lastState = state
        state = newState;
    }

    if (log.level === log.DEBUG) {
        const previous = prevState ? prevState.name : '???';
        const current = state ? state.name : '???';
        const kept = lastState ? lastState.name : '???';
        log.debug(`[state] ${previous} -> ${current} [${kept}]`);
    }
};

const onConnectionReady = () => {
    clearTimeout(sharedSessionFallbackTimer);
    sharedSessionFallbackTimer = null;
    if (room.id) {
        message.show('Joining current session...');
        startGame();
    } else {
        state.menuReady();
    }
};

const parseGameNameFromRoomId = (roomId = '') => {
    const parts = roomId.split('___');
    return parts.length > 1 ? parts[1] : '';
};

const activeGameTitle = (preferRoom = false) => {
    if (preferRoom) {
        return parseGameNameFromRoomId(room.id) || 'Current Session';
    }
    const game = gameListNew.selectedGame;
    if (game) {
        return game.alias || game.title;
    }
    return parseGameNameFromRoomId(room.id) || 'Current Session';
};

const onLatencyCheck = async (data) => {
    message.show('Connecting to fastest server...');
    const servers = await workerManager.checkLatencies(data);
    const latencies = Object.assign({}, ...servers);
    log.info('[ping] <->', latencies);
    api.server.latencyCheck(data.packetId, latencies);
};

const helpScreen = {
    shown: false,
    show: function (show, event) {
        if (this.shown === show) return;

        const isGameScreen = state === app.state.game
        screen.toggle(undefined, !show);

        gui.toggle(keyButtons[KEY.SAVE], show || isGameScreen);
        gui.toggle(keyButtons[KEY.LOAD], show || isGameScreen);

        gui.toggle(helpOverlay, show)

        this.shown = show;

        if (event) pub(HELP_OVERLAY_TOGGLED, {shown: show});
    }
};

// ── New game list screen ──

const showMenuScreen = () => {
    clearTimeout(sharedSessionFallbackTimer);
    sharedSessionFallbackTimer = null;
    log.debug('[control] loading menu screen');

    gui.hide(keyButtons[KEY.SAVE]);
    gui.hide(keyButtons[KEY.LOAD]);

    overlay.disable();

    // Use new game list UI
    gameListNew.show();
    screen.toggle(menu);

    setState(app.state.menu);
};

// Wire up new game list start callback
gameListNew.onStart = () => startGame();

const armSharedSessionFallback = () => {
    clearTimeout(sharedSessionFallbackTimer);
    sharedSessionFallbackTimer = null;

    if (!room.id) return;

    sharedSessionFallbackTimer = setTimeout(() => {
        if (!room.id || state === app.state.game || webrtc.isInputReady()) return;

        log.warn(`[control] shared session attach timed out after ${SHARED_SESSION_FALLBACK_MS}ms; falling back to game list`);
        room.reset();
        message.show('Shared session unavailable. Pick a game.');
        showMenuScreen();
    }, SHARED_SESSION_FALLBACK_MS);
};

// ── Start game ──

const startGame = () => {
    if (!webrtc.isConnected()) {
        // ICE may still be negotiating — wait up to 8s before giving up
        message.show('Connecting...');
        let waited = 0;
        const poll = setInterval(() => {
            waited += 200;
            if (webrtc.isConnected()) {
                clearInterval(poll);
                startGame();
            } else if (waited >= 8000) {
                clearInterval(poll);
                message.show('Game cannot load. Please refresh');
            }
        }, 200);
        return;
    }

    if (!webrtc.isInputReady()) {
        message.show('Game is not ready yet. Please wait');
        return;
    }

    log.info('[control] game start');

    setState(app.state.game);

    // Hide game list, show stream
    gameListNew.hide();
    screen.toggle(stream);

    const joiningSharedSession = !!room.id;
    const selectedTitle = gameListNew.selected || gameList.selected || parseGameNameFromRoomId(room.id);

    api.game.start(
        selectedTitle,
        room.id,
        recording.isActive(),
        recording.getUser(),
        +playerIndex.value - 1,
    )

    gameList.disable();
    gameListNew.disable();

    // Set controller map for this system before enabling retropad.
    // When joining a shared session the user never selected a game, so
    // resolve the game entry from the list using the title encoded in the
    // roomId. Without this, the joining player keeps the default map and
    // can lose the correct analog layout.
    const game = joiningSharedSession
        ? gameListNew.findByTitle(parseGameNameFromRoomId(room.id))
        : gameListNew.selectedGame;
    if (game && game.system) {
        input.joystick.setSystem(game.system);
    }

    input.retropad.toggle(false);
    input.retropad.reset();
    input.retropad.toggle(true);

    // Enable overlay
    overlay.setGameTitle(game ? (game.alias || game.title) : activeGameTitle(joiningSharedSession));
    overlay.setCurrentSlot(+playerIndex.value - 1);
    overlay.enable();
};

const saveGame = debounce(() => api.game.save(), 1000);
const loadGame = debounce(() => api.game.load(), 1000);

// ── Overlay callbacks ──

overlay.onSlotChange = (slot) => {
    updatePlayerIndex(slot);
};

overlay.onSave = () => saveGame();
overlay.onLoad = () => loadGame();
overlay.onReset = () => {
    api.game.reset(room.id);
    message.show("Game reset");
    overlay.close();
};

overlay.onLeave = () => {
    message.show('Killing session...');
    overlay.disable();
    input.retropad.toggle(false);
    api.game.quit(room.id);
};

// ── Late-join slot picker ──

const slotPickerEl = document.getElementById('slot-picker');
const slotPickerBtns = document.querySelectorAll('.slot-picker__btn');

const showSlotPicker = () => {
    slotPickerEl.classList.remove('hidden');
};

const hideSlotPicker = () => {
    slotPickerEl.classList.add('hidden');
};

const resetToMenu = ({reconnect = false} = {}) => {
    clearTimeout(sharedSessionFallbackTimer);
    sharedSessionFallbackTimer = null;
    overlay.disable();
    hideSlotPicker();
    input.retropad.toggle(false);
    room.reset();
    gameList.disable();
    gameListNew.disable();
    showMenuScreen();
    if (reconnect) {
        api.server.initWebrtc();
    }
};

slotPickerBtns.forEach(btn => {
    btn.addEventListener('click', () => {
        const slot = +btn.dataset.slot;
        updatePlayerIndex(slot);
        hideSlotPicker();
        startGame();
    });
});

// ── Message handling ──

// Rumble/haptic feedback via Gamepad Vibration API.
// Track both motors and combine into a single playEffect call,
// since each playEffect replaces the previous one.
const rumbleState = [{strong: 0, weak: 0, timer: null},
                     {strong: 0, weak: 0, timer: null},
                     {strong: 0, weak: 0, timer: null},
                     {strong: 0, weak: 0, timer: null}];

const applyRumble = (port) => {
    const gamepads = navigator.getGamepads ? navigator.getGamepads() : [];
    const gp = gamepads[port];
    if (!gp || !gp.vibrationActuator) return;
    const s = rumbleState[port];
    gp.vibrationActuator.playEffect('dual-rumble', {
        duration: 150,
        strongMagnitude: s.strong,
        weakMagnitude: s.weak,
    }).catch(() => {});
};

const handleRumble = (port, effect, strength) => {
    if (port >= rumbleState.length) return;
    const s = rumbleState[port];
    const intensity = strength / 0xFFFF;
    if (effect === 0) s.strong = intensity;
    else s.weak = intensity;
    // Debounce: wait 5ms for the paired motor update before applying
    if (s.timer) clearTimeout(s.timer);
    s.timer = setTimeout(() => { s.timer = null; applyRumble(port); }, 5);
};

const onMessage = (m) => {
    const {id, t, p: payload} = m;
    switch (t) {
        case api.endpoint.INIT:
            pub(WEBRTC_NEW_CONNECTION, payload);
            break;
        case api.endpoint.OFFER:
            pub(WEBRTC_SDP_OFFER, {sdp: payload});
            break;
        case api.endpoint.ICE_CANDIDATE:
            pub(WEBRTC_ICE_CANDIDATE_RECEIVED, {candidate: payload});
            break;
        case api.endpoint.GAME_START:
            payload.av && pub(APP_VIDEO_CHANGED, payload.av)
            payload.kb_mouse && pub(KB_MOUSE_FLAG)
            pub(GAME_ROOM_AVAILABLE, {roomId: payload.roomId});
            break;
        case api.endpoint.GAME_SAVE:
            pub(GAME_SAVED);
            break;
        case api.endpoint.GAME_LOAD:
            break;
        case api.endpoint.GAME_SET_PLAYER_INDEX:
            pub(GAME_PLAYER_IDX_SET, payload);
            break;
        case api.endpoint.GET_WORKER_LIST:
            pub(WORKER_LIST_FETCHED, payload);
            break;
        case api.endpoint.LATENCY_CHECK:
            pub(LATENCY_CHECK_REQUESTED, {packetId: id, addresses: payload});
            break;
        case api.endpoint.GAME_RECORDING:
            pub(RECORDING_STATUS_CHANGED, payload);
            break;
        case api.endpoint.GAME_ERROR_NO_FREE_SLOTS:
            pub(GAME_ERROR_NO_FREE_SLOTS);
            break;
        case api.endpoint.GAME_QUIT:
            // Server broadcast: the active session was killed by another user.
            // Reset everyone to the game list and reinit WebRTC for the next pick.
            if (state === app.state.game || room.id) {
                message.show('Session ended.');
                webrtc.stop();
                resetToMenu({reconnect: true});
            }
            break;
        case api.endpoint.APP_VIDEO_CHANGE:
            pub(APP_VIDEO_CHANGED, {...payload})
            break;
    }
}

const _dpadArrowKeys = [KEY.UP, KEY.DOWN, KEY.LEFT, KEY.RIGHT];

// pre-state key press handler
const onKeyPress = (data) => {
    const button = keyButtons[data.key];

    if (_dpadArrowKeys.includes(data.key)) {
        button && button.classList.add('dpad-pressed');
    } else {
        if (button) button.classList.add('pressed');
    }

    if (state !== app.state.settings) {
        if (KEY.HELP === data.key) helpScreen.show(true, event);
    }

    state.keyPress(data.key, data.code)
};

// pre-state key release handler
const onKeyRelease = data => {
    const button = keyButtons[data.key];

    if (_dpadArrowKeys.includes(data.key)) {
        button && button.classList.remove('dpad-pressed');
    } else {
        if (button) button.classList.remove('pressed');
    }

    if (state !== app.state.settings) {
        if (KEY.HELP === data.key) helpScreen.show(false, event);
    }

    if (!interacted) {
        stream.audio.mute(false);
        interacted = true;
    }

    if (KEY.SETTINGS === data.key) setState(app.state.settings);

    state.keyRelease(data.key, data.code);
};

const updatePlayerIndex = (idx, not_game = false) => {
    playerIndex.value = idx + 1;
    !not_game && api.game.setPlayerIndex(idx);
    overlay.setCurrentSlot(idx);
};

// noop function for the state
const _nil = () => ({/*_*/})

const onAxisChanged = (data) => {
    if (!interacted) {
        stream.audio.mute(false);
        interacted = true;
    }

    state.axisChanged(data.id, data.value);
};

const onTriggerChanged = (data) => {
    input.retropad.setTriggerChanged(data.id, data.value);
};

const handleToggle = (force = false) => {
    const toggle = document.getElementById('dpad-toggle');

    force && toggle.setAttribute('checked', '')
    toggle.checked = !toggle.checked;
    pub(DPAD_TOGGLE, {checked: toggle.checked});
};

const handleRecording = (data) => {
    const {recording, userName} = data;
    api.game.toggleRecording(recording, userName);
}

const handleRecordingStatus = (data) => {
    if (data === 'ok') {
        message.show(`Recording ${recording.isActive() ? 'on' : 'off'}`)
        if (recording.isActive()) {
            recording.setIndicator(true)
        }
    } else {
        message.show(`Recording failed ):`)
        recording.setIndicator(false)
    }
    log.debug("recording is ", recording.isActive())
}

const _default = {
    name: 'default',
    axisChanged: _nil,
    keyPress: _nil,
    keyRelease: _nil,
    menuReady: _nil,
}
const app = {
    state: {
        eden: {
            ..._default,
            name: 'eden',
            menuReady: showMenuScreen
        },

        settings: {
            ..._default,
            _uber: true,
            name: 'settings',
            keyRelease: (() => {
                settings.ui.onToggle = (o) => !o && setState(lastState);
                return (key) => key === KEY.SETTINGS && settings.ui.toggle()
            })(),
            menuReady: showMenuScreen
        },

        menu: {
            ..._default,
            name: 'menu',
            axisChanged: (id, val) => {
                // Drive new game list with gamepad axis
                if (id === 1) {
                    gameListNew.scroll(val < -.5 ? -1 : val > .5 ? 1 : 0);
                    gameList.scroll(val < -.5 ? -1 : val > .5 ? 1 : 0);
                }
            },
            keyPress: (key) => {
                switch (key) {
                    case KEY.UP:
                    case KEY.DOWN:
                        gameListNew.scroll(key === KEY.UP ? -1 : 1);
                        gameList.scroll(key === KEY.UP ? -1 : 1);
                        break;
                }
            },
            keyRelease: (key) => {
                switch (key) {
                    case KEY.UP:
                    case KEY.DOWN:
                        gameListNew.scroll(0);
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
                        input.retropad.toggle(false)
                        api.game.quit(room.id)
                        break;
                    case KEY.RESET:
                        api.game.reset(room.id)
                        break;
                    case KEY.STATS:
                        stats.toggle();
                        break;
                    case KEY.DTOGGLE:
                        handleToggle();
                        break;
                }
            },
        }
    }
};

// switch keyboard+mouse / retropad
const kbmEl = document.getElementById('kbm')
const kbmEl2 = document.getElementById('kbm2')
let kbmSkip = false
const kbmCb = () => {
    input.kbm = kbmSkip
    kbmSkip = !kbmSkip
    pub(REFRESH_INPUT)
}
gui.multiToggle([kbmEl, kbmEl2], {
    list: [
        {caption: '⌨️+🖱️', cb: kbmCb},
        {caption: ' 🎮 ', cb: kbmCb}
    ]
})
sub(KB_MOUSE_FLAG, () => {
    gui.show(kbmEl, kbmEl2)
    handleToggle(true)
    message.show('Keyboard and mouse work in fullscreen')
})

// Browser lock API
document.onpointerlockchange = () => pub(POINTER_LOCK_CHANGE, document.pointerLockElement)
document.onfullscreenchange = () => pub(FULLSCREEN_CHANGE, document.fullscreenElement)

// subscriptions
sub(MESSAGE, onMessage);

sub(GAME_ROOM_AVAILABLE, async () => {
    stream.play()
}, 2)
sub(GAME_SAVED, () => message.show('Saved'));
sub(GAME_PLAYER_IDX, data => {
    updatePlayerIndex(+data.index, state !== app.state.game);
});
sub(GAME_PLAYER_IDX_SET, idx => {
    if (!isNaN(+idx)) message.show(+idx + 1);
});
sub(GAME_ERROR_NO_FREE_SLOTS, () => message.show("No free slots :(", 2500));
sub(WEBRTC_NEW_CONNECTION, (data) => {
    workerManager.whoami(data.wid);
    webrtc.onData = (x) => {
        // Check for binary rumble messages (prefix 0xFF)
        if (x.data instanceof ArrayBuffer) {
            const bytes = new Uint8Array(x.data);
            if (bytes.length >= 5 && bytes[0] === 0xFF) {
                handleRumble(bytes[1], bytes[2], (bytes[3] << 8) | bytes[4]);
                return;
            }
        }
        onMessage(api.decode(x.data))
    }
    webrtc.start(data.ice);
    if (data.roomId) {
        room.id = data.roomId;
    }
    api.server.initWebrtc()
    // Set games immediately — show menu without waiting for WebRTC
    gameList.set(data.games);
    gameListNew.set(data.games);
    // Show the game list as soon as we have the game data
    if (!data.roomId && (state === app.state.eden || state === app.state.menu)) {
        showMenuScreen();
    } else if (data.roomId) {
        armSharedSessionFallback();
    }
});
sub(WEBRTC_ICE_CANDIDATE_FOUND, (data) => api.server.sendIceCandidate(data.candidate));
sub(WEBRTC_SDP_ANSWER, (data) => api.server.sendSdp(data.sdp));
sub(WEBRTC_SDP_OFFER, (data) => webrtc.setRemoteDescription(data.sdp, stream.video.el));
sub(WEBRTC_ICE_CANDIDATE_RECEIVED, (data) => webrtc.addCandidate(data.candidate));
sub(WEBRTC_ICE_CANDIDATES_FLUSH, () => webrtc.flushCandidates());
sub(WEBRTC_CONNECTION_READY, onConnectionReady);
sub(WEBRTC_CONNECTION_CLOSED, () => {
    webrtc.stop();
    if (state === app.state.game) {
        // In-game disconnect: reset fully and reconnect.
        resetToMenu({reconnect: true});
    } else if (state !== app.state.eden && room.id) {
        // Mid-session disconnect (not fresh page load): reset and reconnect.
        resetToMenu({reconnect: true});
    } else {
        // Fresh page load or already at menu: don't wipe room.id.
        // The new InitSession may have already set it.
        input.retropad.toggle(false);
    }
});
sub(LATENCY_CHECK_REQUESTED, onLatencyCheck);
sub(GAMEPAD_CONNECTED, () => message.show('Gamepad connected'));
sub(GAMEPAD_DISCONNECTED, () => message.show('Gamepad disconnected'));

// keyboard handler in the Screen Lock mode
sub(KEYBOARD_KEY_DOWN, (v) => state.keyboardInput?.(true, v))
sub(KEYBOARD_KEY_UP, (v) => state.keyboardInput?.(false, v))

// mouse handler in the Screen Lock mode
sub(MOUSE_MOVED, (e) => state.mouseMove?.(e))
sub(MOUSE_PRESSED, (e) => state.mousePress?.(e))

// general keyboard handler
sub(KEY_PRESSED, onKeyPress);
sub(KEY_RELEASED, onKeyRelease);

sub(SETTINGS_CHANGED, () => message.show('Settings have been updated'));
sub(AXIS_CHANGED, onAxisChanged);
sub(TRIGGER_CHANGED, onTriggerChanged);
sub(CONTROLLER_UPDATED, data => webrtc.input(data));
sub(RECORDING_TOGGLED, handleRecording);
sub(RECORDING_STATUS_CHANGED, handleRecordingStatus);

sub(SETTINGS_CHANGED, () => {
    const s = settings.get();
    log.level = s[opts.LOG_LEVEL];
});

// ── Orphaned-session recovery button ──
// Hidden for the first ORPHAN_DELAY_MS so normal fast connections don't flash it.
// After the delay, if still in "Connecting..." (.loading), it becomes visible.
// Disappears permanently once the game list populates (loading class is removed).
// Clicking reloads without a room ID so coordinator assigns a fresh worker.
(function initOrphanRecover() {
    const ORPHAN_DELAY_MS = 6000;
    const btn = document.getElementById('orphan-recover-btn');
    if (!btn) return;

    // Start hidden via inline style (overrides the CSS .loading rule during grace period)
    btn.style.display = 'none';

    const screenEl = document.getElementById('game-list-screen');

    // After delay, reveal if still in connecting state
    const showTimer = setTimeout(() => {
        if (screenEl && screenEl.classList.contains('loading')) {
            btn.style.removeProperty('display'); // let CSS .loading rule take effect → display:block
        }
    }, ORPHAN_DELAY_MS);

    // Hide permanently once game list loads (loading class removed from parent)
    const observer = new MutationObserver(() => {
        if (!screenEl.classList.contains('loading')) {
            clearTimeout(showTimer);
            btn.style.display = 'none';
            observer.disconnect();
        }
    });
    if (screenEl) {
        observer.observe(screenEl, {attributes: true, attributeFilter: ['class']});
    }

    btn.addEventListener('click', () => {
        // Clear any stored room ID so we reconnect fresh (no stale room join)
        try { localStorage.removeItem('roomID'); } catch (_) {}
        // Navigate to clean URL (no ?id= param) — browser closes the WS on unload,
        // which causes coordinator to RemoveDisconnect and release the worker association.
        const cleanUrl = window.location.origin + window.location.pathname;
        window.location.replace(cleanUrl);
    });
})();

// initial app state
setState(app.state.eden);

input.init()

stream.init();
screen.init();

let [roomId, zone] = room.loadMaybe();
// find worker id if present
const wid = new URLSearchParams(document.location.search).get('wid');
// if from URL -> start game immediately!
socket.init(roomId, wid, zone);
api.transport = {
    send: socket.send,
    keyboard: webrtc.keyboard,
    mouse: webrtc.mouse,
}

// stats
let WEBRTC_STATS_RTT;
let VIDEO_BITRATE;
let GET_V_CODEC, SET_CODEC;

const bitrate = (() => {
    let bytesPrev, timestampPrev
    const w = [0, 0, 0, 0, 0, 0]
    const n = w.length
    let i = 0
    return (now, bytes) => {
        w[i++ % n] = timestampPrev ? Math.floor(8 * (bytes - bytesPrev) / (now - timestampPrev)) : 0
        bytesPrev = bytes
        timestampPrev = now
        return Math.floor(w.reduce((a, b) => a + b) / n)
    }
})()

stats.modules = [
    {
        mui: stats.mui('', '<1'),
        init() {
            WEBRTC_STATS_RTT = (v) => (this.val = v)
        },
    },
    {
        mui: stats.mui('', '', false, () => ''),
        init() {
            GET_V_CODEC = (v) => (this.val = v + ' @ ')
        }
    },
    {
        mui: stats.mui('', '', false, () => ''),
        init() {
            sub(APP_VIDEO_CHANGED, ({s = 1, w, h}) => (this.val = `${w * s}x${h * s}`))
        },
    },
    {
        mui: stats.mui('', '', false, () => ' kb/s', 'stats-bitrate'),
        init() {
            VIDEO_BITRATE = (v) => (this.val = v)
        }
    },
    {
        async stats() {
            const stats = await webrtc.stats();
            if (!stats) return;

            stats.forEach(report => {
                if (!SET_CODEC && report.mimeType?.startsWith('video/')) {
                    GET_V_CODEC(report.mimeType.replace('video/', '').toLowerCase())
                    SET_CODEC = 1
                }
                const {nominated, currentRoundTripTime, type, kind} = report;
                if (nominated && currentRoundTripTime !== undefined) {
                    WEBRTC_STATS_RTT(currentRoundTripTime * 1000);
                }
                if (type === 'inbound-rtp' && kind === 'video') {
                    VIDEO_BITRATE(bitrate(report.timestamp, report.bytesReceived))
                }
            });
        },
        enable() {
            this.interval = window.setInterval(this.stats, 999);
        },
        disable() {
            window.clearInterval(this.interval);
        },
    }]

stats.toggle()
