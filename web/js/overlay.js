/**
 * Overlay UI module.
 *
 * Manages the in-game overlay panel (desktop mouse-idle + mobile cog),
 * the desktop hover bar, player slot display, and overlay actions.
 *
 * Slot + roster state lives in state.js; this module subscribes to
 * rerender when either changes.
 */

import {getState, subscribe} from 'state';

const panelEl = document.getElementById('overlay-panel');
const cogEl = document.getElementById('overlay-cog');
const gameTitleEl = document.getElementById('overlay-game-title');
const playersEl = document.getElementById('overlay-players');
const slotButtons = playersEl.querySelectorAll('.overlay-player-slot');
const audioBtn = document.getElementById('overlay-audio');
const inviteBtn = document.getElementById('overlay-invite');
const saveBtn = document.getElementById('overlay-save');
const loadBtn = document.getElementById('overlay-load');
const resetBtn = document.getElementById('overlay-reset');
const leaveBtn = document.getElementById('overlay-leave');
const blowBtn = document.getElementById('overlay-blow-on-cartridge');

let backdropEl = null;

let isOpen = false;
// Is a game currently running? Only toggles the panel's in-game sections
// (players, save/load, reset, leave). The cog itself is always visible
// so "Blow on the Cartridge" + Preferences are reachable in menu too.
let inGame = false;

// Desktop: mouse idle timer
let mouseIdleTimer = null;
const MOUSE_IDLE_MS = 3000;

// Detect mobile
const isMobile = () =>
    matchMedia('(hover: none) and (pointer: coarse)').matches ||
    window.innerWidth <= 768;

// Callbacks set by app.js
let onSlotChange = () => {};
let onInvite = () => {};
let onSave = () => {};
let onLoad = () => {};
let onReset = () => {};
let onLeave = () => {};
let onAudio = () => {};
let onBlowOnCartridge = () => {};

const createBackdrop = () => {
    if (backdropEl) return backdropEl;
    backdropEl = document.createElement('div');
    backdropEl.className = 'overlay-backdrop';
    backdropEl.addEventListener('click', () => close());
    return backdropEl;
};

const open = () => {
    if (isOpen) return;
    isOpen = true;

    // Insert backdrop before panel
    document.getElementById('gamebody').appendChild(createBackdrop());
    panelEl.classList.remove('hidden');

    updateSlots();
};

const close = () => {
    if (!isOpen) return;
    isOpen = false;

    panelEl.classList.add('hidden');
    if (backdropEl && backdropEl.parentNode) {
        backdropEl.parentNode.removeChild(backdropEl);
    }
};

const toggle = () => isOpen ? close() : open();

const setGameTitle = (title) => {
    gameTitleEl.textContent = title || '—';
};

// Deterministic background colour from a subject string so the same user
// gets the same avatar colour across sessions and across all clients.
// 8 hand-picked hues with enough separation to be distinguishable next
// to each other when stacked in a single slot.
const avatarColors = [
    '#e74c3c', '#3498db', '#2ecc71', '#9b59b6',
    '#f39c12', '#1abc9c', '#e67e22', '#8e44ad',
];
const colourFor = (sub) => {
    if (!sub) return avatarColors[0];
    let h = 0;
    for (let i = 0; i < sub.length; i++) h = (h * 31 + sub.charCodeAt(i)) | 0;
    return avatarColors[Math.abs(h) % avatarColors.length];
};

// Render a single avatar element for one room member.
const makeAvatar = (member) => {
    const el = document.createElement('span');
    el.className = 'slot-avatar';
    el.title = member.identity?.username || member.identity?.email || 'anonymous';
    const pic = member.identity?.picture;
    if (pic) {
        el.style.backgroundImage = `url(${pic})`;
        el.classList.add('slot-avatar--img');
    } else {
        const sub = member.identity?.sub || member.user_id || '';
        const label = (member.identity?.username || sub || '?').trim().charAt(0).toUpperCase() || '?';
        el.textContent = label;
        el.style.backgroundColor = colourFor(sub);
    }
    return el;
};

const updateSlots = () => {
    const {currentSlot, roomMembers} = getState();
    // Bucket members by slot so each slot button can stack its occupants.
    const bySlot = new Map();
    for (const m of roomMembers) {
        if (!bySlot.has(m.slot)) bySlot.set(m.slot, []);
        bySlot.get(m.slot).push(m);
    }
    slotButtons.forEach(btn => {
        const slot = +btn.dataset.slot;
        const occupants = bySlot.get(slot) || [];
        btn.classList.toggle('is-current', slot === currentSlot);
        btn.classList.toggle('is-occupied', occupants.length > 0);

        // Replace (not append) the avatar stack so we don't accumulate
        // on repeat renders. Avatars live before .slot-label in DOM.
        let stack = btn.querySelector('.slot-avatars');
        if (!stack) {
            stack = document.createElement('span');
            stack.className = 'slot-avatars';
            btn.insertBefore(stack, btn.firstChild);
        }
        stack.replaceChildren(...occupants.map(makeAvatar));

        // Flip the ○/● indicator that lives in .slot-status to reflect
        // occupancy. Kept alongside the avatar stack so the slot shows
        // as "claimed" even when the avatar URL hasn't loaded yet.
        const status = btn.querySelector('.slot-status');
        if (status) status.textContent = occupants.length > 0 ? '●' : '○';
    });
};

// Re-render whenever currentSlot or roomMembers changes in the store.
// We're coarse on purpose — updateSlots is idempotent and cheap.
subscribe(() => { if (isOpen || inGame) updateSlots(); });

// ── Desktop mouse-move overlay bar ──
// We reuse the cog/panel approach but auto-show on mouse move on desktop
// when a game is active. Outside a game the cog is always visible (no
// need to hide-and-reveal on movement since the menu doesn't compete
// for attention with streaming gameplay).
const onMouseMove = () => {
    if (!inGame) return;
    if (isMobile()) return;

    cogEl.classList.add('visible');

    clearTimeout(mouseIdleTimer);
    mouseIdleTimer = setTimeout(() => {
        if (!isOpen) {
            cogEl.classList.remove('visible');
        }
    }, MOUSE_IDLE_MS);
};

// ── Event wiring ──

// Cog click
cogEl.addEventListener('click', (e) => {
    e.stopPropagation();
    toggle();
});

// Slot buttons in overlay panel. onSlotChange is the authoritative
// writer — it lands in session.updatePlayerIndex, which pushes the
// new slot into state.js, and the subscribe() below re-renders. No
// local write needed.
slotButtons.forEach(btn => {
    btn.addEventListener('click', () => {
        onSlotChange(+btn.dataset.slot);
    });
});

inviteBtn.addEventListener('click', () => {
    onInvite();
    inviteBtn.textContent = 'Link copied!';
    setTimeout(() => { inviteBtn.textContent = 'Invite Friends'; }, 2000);
});

saveBtn.addEventListener('click', () => onSave());
loadBtn.addEventListener('click', () => onLoad());
resetBtn.addEventListener('click', () => onReset());
leaveBtn.addEventListener('click', () => onLeave());
blowBtn.addEventListener('click', () => {
    // Closes the panel immediately so the click visually "takes" before
    // the teardown runs; the reset itself is slow-ish (WebSocket close
    // + reopen + InitSession) and we don't want a stuck-looking panel.
    close();
    onBlowOnCartridge();
});

// The audio button's click handler MUST call video.play() / muted=false
// directly inside this handler — Safari requires the user-gesture chain
// to reach the media API, so the app.js callback must run synchronously.
audioBtn.addEventListener('click', () => onAudio());

// Desktop: Escape to close
document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && isOpen) {
        close();
        e.preventDefault();
    }
});

// Desktop: mouse move on gamebody
document.getElementById('gamebody').addEventListener('mousemove', onMouseMove);

/**
 * Overlay module export.
 */
export const overlay = {
    /**
     * enable() — call when entering game state. Reveals the in-game
     * sections (players, save/load, reset, leave) and the desktop
     * mouse-idle cog behaviour. The cog itself is always visible; this
     * just flips the panel into "in-game" mode.
     */
    enable() {
        inGame = true;
        panelEl.classList.add('is-in-game');
        // In-game on desktop: cog hides until mouse moves, managed by
        // onMouseMove below. On mobile the cog is always visible per CSS.
        cogEl.classList.remove('is-static');
        if (isMobile()) {
            cogEl.classList.add('visible');
        }
    },
    /**
     * disable() — call when leaving game state. Hides the in-game
     * sections so only the universal actions (Preferences, Blow on the
     * Cartridge) remain in the panel. The cog stays available.
     */
    disable() {
        inGame = false;
        panelEl.classList.remove('is-in-game');
        cogEl.classList.remove('visible');
        // Pin cog open in menu state so "Blow on the Cartridge" is
        // reachable whenever nothing is streaming — in-game attention
        // auto-hide doesn't make sense when there's no game.
        cogEl.classList.add('is-static');
        close();
        clearTimeout(mouseIdleTimer);
    },
    open,
    close,
    toggle,
    get isOpen() { return isOpen; },
    setGameTitle,

    set onSlotChange(fn) { onSlotChange = fn; },
    set onInvite(fn) { onInvite = fn; },
    set onSave(fn) { onSave = fn; },
    set onLoad(fn) { onLoad = fn; },
    set onReset(fn) { onReset = fn; },
    set onLeave(fn) { onLeave = fn; },
    set onAudio(fn) { onAudio = fn; },
    set onBlowOnCartridge(fn) { onBlowOnCartridge = fn; },
    /** Update audio-button label to reflect current muted state. */
    setAudioMuted(muted) {
        audioBtn.textContent = muted ? '🔊 Enable Audio' : '🔇 Mute Audio';
    },
};
