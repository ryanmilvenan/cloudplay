// Event bus wiring: every pub/sub subscription, the onMessage switch
// that translates incoming WebRTC endpoint codes into app events, and
// a handful of direct document-level handlers (pointerlock, fullscreen,
// kbm toggle). By convention, sub() calls live here and nowhere else —
// grep for a target event name to trace who listens to what.

import {api} from 'api';
import {gui} from 'gui';
import {input} from 'input';
import {log} from 'log';
import {opts, settings} from 'settings';
import {webrtc} from 'network';
import {
    APP_VIDEO_CHANGED,
    AXIS_CHANGED,
    CONTROLLER_UPDATED,
    GAME_ERROR_NO_FREE_SLOTS,
    GAME_PLAYER_IDX,
    GAME_PLAYER_IDX_SET,
    GAME_ROOM_AVAILABLE,
    GAME_SAVED,
    GAMEPAD_CONNECTED,
    GAMEPAD_DISCONNECTED,
    KB_MOUSE_FLAG,
    KEY_PRESSED,
    KEY_RELEASED,
    KEYBOARD_KEY_DOWN,
    KEYBOARD_KEY_UP,
    LATENCY_CHECK_REQUESTED,
    MESSAGE,
    MOUSE_MOVED,
    MOUSE_PRESSED,
    RECORDING_STATUS_CHANGED,
    RECORDING_TOGGLED,
    REFRESH_INPUT,
    SETTINGS_CHANGED,
    TRIGGER_CHANGED,
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

import {gameList} from '../gameList.js?v=__V__';
import {message} from '../message.js?v=__V__';
import {overlay} from '../overlay.js?v=__V__';
import {recording} from '../recording.js?v=__V__';
import {room} from '../room.js?v=__V__';
import {stream} from '../stream.js?v=__V__';
import {workerManager} from '../workerManager.js?v=__V__';

import {handleRumble} from './rumble.js?v=__V__';
import {getState, setState} from 'state';
import {app, armSharedSessionFallback, onConnectionReady, showMenuScreen} from './lifecycle.js?v=__V__';
import {
    handleToggle,
    onAxisChanged,
    onKeyPress,
    onKeyRelease,
    onTriggerChanged,
} from './keys.js?v=__V__';
import {
    onLatencyCheck,
    resetToMenu,
    updatePlayerIndex,
} from './session.js?v=__V__';

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
            payload.av && pub(APP_VIDEO_CHANGED, payload.av);
            payload.kb_mouse && pub(KB_MOUSE_FLAG);
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
            // Broadcast: active session killed by another user. Reset
            // everyone to the game list and reinit WebRTC for the next
            // pick.
            if (getState().appState === app.state.game || room.id) {
                message.show('Session ended.');
                webrtc.stop();
                resetToMenu({reconnect: true});
            }
            break;
        case api.endpoint.APP_VIDEO_CHANGE:
            pub(APP_VIDEO_CHANGED, {...payload});
            break;
        case api.endpoint.ROOM_MEMBERS:
            // Full-roster snapshot from the worker: { members: [{ user_id, slot, identity }] }
            setState({roomMembers: payload?.members || []});
            break;
    }
};

const handleRecording = (data) => {
    const {recording: rec, userName} = data;
    api.game.toggleRecording(rec, userName);
};

const handleRecordingStatus = (data) => {
    if (data === 'ok') {
        message.show(`Recording ${recording.isActive() ? 'on' : 'off'}`);
        if (recording.isActive()) recording.setIndicator(true);
    } else {
        message.show(`Recording failed ):`);
        recording.setIndicator(false);
    }
    log.debug('recording is ', recording.isActive());
};

export const initWiring = () => {
    // Keyboard + mouse / retropad toggle
    const kbmEl = document.getElementById('kbm');
    const kbmEl2 = document.getElementById('kbm2');
    let kbmSkip = false;
    const kbmCb = () => {
        input.kbm = kbmSkip;
        kbmSkip = !kbmSkip;
        pub(REFRESH_INPUT);
    };
    gui.multiToggle([kbmEl, kbmEl2], {
        list: [
            {caption: '⌨️+🖱️', cb: kbmCb},
            {caption: ' 🎮 ', cb: kbmCb},
        ],
    });
    sub(KB_MOUSE_FLAG, () => {
        gui.show(kbmEl, kbmEl2);
        handleToggle(true);
        message.show('Keyboard and mouse work in fullscreen');
    });

    // Incoming messages
    sub(MESSAGE, onMessage);

    sub(GAME_ROOM_AVAILABLE, async () => { stream.play(); }, 2);
    sub(GAME_SAVED, () => message.show('Saved'));
    sub(GAME_PLAYER_IDX, data => {
        updatePlayerIndex(+data.index, getState().appState !== app.state.game);
    });
    sub(GAME_PLAYER_IDX_SET, idx => {
        if (!isNaN(+idx)) message.show(+idx + 1);
    });
    sub(GAME_ERROR_NO_FREE_SLOTS, () => message.show('No free slots :(', 2500));
    sub(WEBRTC_NEW_CONNECTION, (data) => {
        workerManager.whoami(data.wid);
        webrtc.onData = (x) => {
            // Binary rumble messages (prefix 0xFF + port + effect + strength_hi + strength_lo)
            if (x.data instanceof ArrayBuffer) {
                const bytes = new Uint8Array(x.data);
                if (bytes.length >= 5 && bytes[0] === 0xFF) {
                    handleRumble(bytes[1], bytes[2], (bytes[3] << 8) | bytes[4]);
                    return;
                }
            }
            onMessage(api.decode(x.data));
        };
        webrtc.start(data.ice);
        if (data.roomId) room.id = data.roomId;
        api.server.initWebrtc();
        gameList.set(data.games);
        if (!data.roomId && (getState().appState === app.state.eden || getState().appState === app.state.menu)) {
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
        if (getState().appState === app.state.game) {
            resetToMenu({reconnect: true});
        } else if (getState().appState !== app.state.eden && room.id) {
            resetToMenu({reconnect: true});
        } else {
            // Fresh page load or already at menu: preserve room.id for pending InitSession.
            input.retropad.toggle(false);
        }
    });
    sub(LATENCY_CHECK_REQUESTED, onLatencyCheck);
    sub(GAMEPAD_CONNECTED, () => message.show('Gamepad connected'));
    sub(GAMEPAD_DISCONNECTED, () => message.show('Gamepad disconnected'));

    // Keyboard + mouse in Screen Lock mode — forwarded to state.keyboardInput / mouseMove / mousePress.
    sub(KEYBOARD_KEY_DOWN, (v) => getState().appState.keyboardInput?.(true, v));
    sub(KEYBOARD_KEY_UP, (v) => getState().appState.keyboardInput?.(false, v));
    sub(MOUSE_MOVED, (e) => getState().appState.mouseMove?.(e));
    sub(MOUSE_PRESSED, (e) => getState().appState.mousePress?.(e));

    // General keyboard handler
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
};
