// User-specific in-game preferences panel. Opens from the overlay
// cog's "⚙ Preferences" button. Per-user, per-platform settings:
// core options (e.g. N64 controller paks), RetroAchievements
// credential, control overrides.
//
// Scaffold only — content sections are placeholders until subsequent
// commits wire them up. Identity render reacts to state changes via
// subscribe() so it updates the moment InitSession payload lands.

import {getState, subscribe} from 'state';

const panelEl = document.getElementById('user-settings-panel');
const openBtn = document.getElementById('overlay-user-settings');
const closeBtn = document.getElementById('user-settings-close');
const identityEl = document.getElementById('user-settings-identity');

let isOpen = false;

const renderIdentity = () => {
    const {identity} = getState();
    if (!identity || !identity.sub) {
        identityEl.textContent = 'Anonymous';
        return;
    }
    identityEl.textContent = identity.username || identity.email || identity.sub;
};

const open = () => {
    panelEl.classList.remove('hidden');
    isOpen = true;
    renderIdentity();
};

const close = () => {
    panelEl.classList.add('hidden');
    isOpen = false;
};

const toggle = () => (isOpen ? close() : open());

openBtn.addEventListener('click', toggle);
closeBtn.addEventListener('click', close);

// Close on Escape when the panel is open.
document.addEventListener('keydown', (e) => {
    if (isOpen && e.key === 'Escape') close();
});

// Keep identity in sync if the user is looking at the panel and the
// value arrives late (e.g. after INIT in a future commit).
subscribe(() => { if (isOpen) renderIdentity(); });

export const userSettings = {open, close, toggle, get isOpen() { return isOpen; }};
