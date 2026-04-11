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

const updateSlots = () => {
    slotButtons.forEach(btn => {
        const slot = +btn.dataset.slot;
        btn.classList.toggle('is-current', slot === currentSlot);
    });
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

    set onSlotChange(fn) { onSlotChange = fn; },
    set onInvite(fn) { onInvite = fn; },
    set onSave(fn) { onSave = fn; },
    set onLoad(fn) { onLoad = fn; },
    set onReset(fn) { onReset = fn; },
    set onLeave(fn) { onLeave = fn; },
};
