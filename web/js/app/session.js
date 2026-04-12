// Session lifecycle: starting and quitting games, slot-picker flow,
// overlay callbacks (save / load / reset / leave / audio / slot-change),
// and the WebRTC-readiness poll. Owns everything that happens between
// "user picks a game" and "user leaves the session".

import {api} from 'api';
import {input} from 'input';
import {webrtc} from 'network';
import {debounce} from 'utils';
import {log} from 'log';

import {gameList} from '../gameList.js?v=__V__';
import {message} from '../message.js?v=__V__';
import {overlay} from '../overlay.js?v=__V__';
import {recording} from '../recording.js?v=__V__';
import {room} from '../room.js?v=__V__';
import {screen} from '../screen.js?v=__V__';
import {stream} from '../stream.js?v=__V__';
import {workerManager} from '../workerManager.js?v=__V__';

import {getState, setState, setAppState} from 'state';
import {app, showMenuScreen, cancelSharedSessionFallback} from './lifecycle.js?v=__V__';

const playerIndex = document.getElementById('playeridx');

export const parseGameNameFromRoomId = (roomId = '') => {
    const parts = roomId.split('___');
    return parts.length > 1 ? parts[1] : '';
};

export const activeGameTitle = (preferRoom = false) => {
    if (preferRoom) {
        return parseGameNameFromRoomId(room.id) || 'Current Session';
    }
    const game = gameList.selectedGame;
    if (game) return game.alias || game.title;
    return parseGameNameFromRoomId(room.id) || 'Current Session';
};

export const startGame = () => {
    if (!webrtc.isConnected()) {
        // ICE may still be negotiating — wait up to 8s before giving up.
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

    setAppState(app.state.game);

    gameList.hide();
    screen.toggle(stream);

    const joiningSharedSession = !!room.id;
    setState({joiningSharedSession});
    const selectedTitle = gameList.selected || parseGameNameFromRoomId(room.id);

    api.game.start(
        selectedTitle,
        room.id,
        recording.isActive(),
        recording.getUser(),
        +playerIndex.value - 1,
    );

    gameList.disable();

    // When joining a shared session the user never selected a game, so
    // resolve the game entry from the title encoded in the roomId —
    // otherwise the joining player keeps the default controller map.
    const game = joiningSharedSession
        ? gameList.findByTitle(parseGameNameFromRoomId(room.id))
        : gameList.selectedGame;
    if (game && game.system) {
        input.joystick.setSystem(game.system);
    }

    input.retropad.toggle(false);
    input.retropad.reset();
    input.retropad.toggle(true);

    overlay.setGameTitle(game ? (game.alias || game.title) : activeGameTitle(joiningSharedSession));
    setState({currentSlot: +playerIndex.value - 1});
    overlay.enable();
};

export const saveGame = debounce(() => api.game.save(), 1000);
export const loadGame = debounce(() => api.game.load(), 1000);

// --- Slot picker ---

const slotPickerEl = document.getElementById('slot-picker');
const slotPickerBtns = document.querySelectorAll('.slot-picker__btn');

export const showSlotPicker = () => slotPickerEl.classList.remove('hidden');
export const hideSlotPicker = () => slotPickerEl.classList.add('hidden');

export const updatePlayerIndex = (idx, not_game = false) => {
    playerIndex.value = idx + 1;
    !not_game && api.game.setPlayerIndex(idx);
    setState({currentSlot: idx});
};

export const resetToMenu = ({reconnect = false} = {}) => {
    cancelSharedSessionFallback();
    overlay.disable();
    hideSlotPicker();
    input.retropad.toggle(false);
    room.reset();
    gameList.disable();
    showMenuScreen();
    if (reconnect) api.server.initWebrtc();
};

slotPickerBtns.forEach(btn => {
    btn.addEventListener('click', () => {
        const slot = +btn.dataset.slot;
        updatePlayerIndex(slot);
        hideSlotPicker();
        startGame();
    });
});

// --- Overlay callbacks ---

gameList.onStart = () => startGame();

overlay.onSlotChange = (slot) => updatePlayerIndex(slot);
overlay.onSave = () => saveGame();
overlay.onLoad = () => loadGame();
overlay.onReset = () => {
    api.game.reset(room.id);
    message.show('Game reset');
    overlay.close();
};
overlay.onLeave = () => {
    message.show('Killing session...');
    overlay.disable();
    input.retropad.toggle(false);
    api.game.quit(room.id);
};

// Safari workaround: #audio-btn doesn't render reliably and Safari's
// autoplay policy requires a user gesture to unmute a video that started
// muted. Route through the cog panel — tapping this button inside the
// click handler keeps the user-gesture chain intact.
overlay.onAudio = () => {
    const video = stream.video.el;
    const willMute = !video.muted;
    stream.audio.mute(willMute);
    if (!willMute) stream.play();
    setState({interacted: true});
    overlay.setAudioMuted(video.muted);
};

export const onLatencyCheck = async (data) => {
    message.show('Connecting to fastest server...');
    const servers = await workerManager.checkLatencies(data);
    const latencies = Object.assign({}, ...servers);
    log.info('[ping] <->', latencies);
    api.server.latencyCheck(data.packetId, latencies);
};
