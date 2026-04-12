/**
 * Overlay UI module.
 *
 * Manages the in-game overlay panel (desktop mouse-idle + mobile cog),
 * the desktop hover bar, player slot display, and overlay actions.
 */

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

let backdropEl = null;

let currentSlot = 0;
let isOpen = false;
let enabled = false; // only active in game state

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

const setCurrentSlot = (idx) => {
    currentSlot = idx;
    if (isOpen) updateSlots();
};

// roomMembers is the last roster broadcast received from the worker.
// Structure: [{ user_id, slot, identity: { sub, username, picture, ... } }, ...]
// Multiple members can share a slot (free-form assignment is intentional).
let roomMembers = [];

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

// Public: called by app.js when the worker broadcasts a new roster.
const setRoomMembers = (members) => {
    roomMembers = Array.isArray(members) ? members : [];
    updateSlots();
};

// ── Desktop mouse-move overlay bar ──
// We reuse the cog/panel approach but auto-show on mouse move on desktop
const onMouseMove = () => {
    if (!enabled) return;
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

// Slot buttons in overlay panel
slotButtons.forEach(btn => {
    btn.addEventListener('click', () => {
        const slot = +btn.dataset.slot;
        currentSlot = slot;
        updateSlots();
        onSlotChange(slot);
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
    /** Enable overlay (call when entering game state) */
    enable() {
        enabled = true;
        cogEl.classList.remove('hidden');
        if (isMobile()) {
            cogEl.classList.add('visible');
        }
    },
    /** Disable overlay (call when leaving game state) */
    disable() {
        enabled = false;
        cogEl.classList.add('hidden');
        cogEl.classList.remove('visible');
        close();
        clearTimeout(mouseIdleTimer);
    },
    open,
    close,
    toggle,
    get isOpen() { return isOpen; },
    setGameTitle,
    setCurrentSlot,
    setRoomMembers,

    set onSlotChange(fn) { onSlotChange = fn; },
    set onInvite(fn) { onInvite = fn; },
    set onSave(fn) { onSave = fn; },
    set onLoad(fn) { onLoad = fn; },
    set onReset(fn) { onReset = fn; },
    set onLeave(fn) { onLeave = fn; },
    set onAudio(fn) { onAudio = fn; },
    /** Update audio-button label to reflect current muted state. */
    setAudioMuted(muted) {
        audioBtn.textContent = muted ? '🔊 Enable Audio' : '🔇 Mute Audio';
    },
};
