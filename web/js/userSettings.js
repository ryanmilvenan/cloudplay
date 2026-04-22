// User-specific in-game preferences panel. Opens from the overlay
// cog's "⚙ Preferences" button. Per-user, per-platform settings:
// core options (e.g. N64 controller paks), RetroAchievements
// credential, control overrides.
//
// Scaffold only — content sections are placeholders until subsequent
// commits wire them up. Identity render reacts to state changes via
// subscribe() so it updates the moment InitSession payload lands.

import {api} from 'api';
import {getState, subscribe} from 'state';
import {opts, settings} from 'settings';

const panelEl = document.getElementById('user-settings-panel');
const openBtn = document.getElementById('overlay-user-settings');
const closeBtn = document.getElementById('user-settings-close');
const identityEl = document.getElementById('user-settings-identity');
const micEnabledEl = document.getElementById('user-settings-mic-enabled');
const raUserEl = document.getElementById('user-settings-ra-user');
const raTokenEl = document.getElementById('user-settings-ra-token');
const raSaveEl = document.getElementById('user-settings-ra-save');
const raClearEl = document.getElementById('user-settings-ra-clear');
const raStatusEl = document.getElementById('user-settings-ra-status');

let isOpen = false;

// RA credentials are stored per pocket-id sub. Anonymous users get a
// single shared bucket ("anon") — acceptable since anonymous is a
// dev / bypass-only mode.
const raStorageKey = (sub) => `ra:${sub || 'anon'}`;

const readRaCredentials = () => {
    const {identity} = getState();
    try {
        const raw = localStorage.getItem(raStorageKey(identity?.sub));
        return raw ? JSON.parse(raw) : null;
    } catch (_) { return null; }
};

const writeRaCredentials = (val) => {
    const {identity} = getState();
    if (val) {
        localStorage.setItem(raStorageKey(identity?.sub), JSON.stringify(val));
    } else {
        localStorage.removeItem(raStorageKey(identity?.sub));
    }
};

const setRaStatus = (text, kind = '') => {
    raStatusEl.textContent = text;
    raStatusEl.classList.remove('is-ok', 'is-err');
    if (kind) raStatusEl.classList.add(`is-${kind}`);
};

const renderIdentity = () => {
    const {identity} = getState();
    if (!identity || !identity.sub) {
        identityEl.textContent = 'Anonymous';
    } else {
        identityEl.textContent = identity.username || identity.email || identity.sub;
    }
};

const renderRa = () => {
    const creds = readRaCredentials();
    if (creds) {
        raUserEl.value = creds.user || '';
        raTokenEl.value = creds.token || '';
        setRaStatus(creds.user ? `Saved as ${creds.user}` : 'Saved', 'ok');
    } else {
        raUserEl.value = '';
        raTokenEl.value = '';
        setRaStatus('Not set');
    }
};

const renderMic = () => {
    micEnabledEl.checked = !!settings.loadOr(opts.ENABLE_MICROPHONE, false);
};

const open = () => {
    panelEl.classList.remove('hidden');
    isOpen = true;
    renderIdentity();
    renderMic();
    renderRa();
};

const close = () => {
    panelEl.classList.add('hidden');
    isOpen = false;
};

const toggle = () => (isOpen ? close() : open());

openBtn.addEventListener('click', toggle);
closeBtn.addEventListener('click', close);

micEnabledEl.addEventListener('change', (e) => {
    settings.set(opts.ENABLE_MICROPHONE, !!e.target.checked);
});

raSaveEl.addEventListener('click', () => {
    const user = raUserEl.value.trim();
    const token = raTokenEl.value.trim();
    if (!user || !token) {
        setRaStatus('Username and token required', 'err');
        return;
    }
    writeRaCredentials({user, token});
    // Push to worker so it can log into rcheevos. Fire-and-forget —
    // the worker logs success/failure; a future commit can surface
    // that back here.
    try { api.server.setRaCredentials(user, token); } catch (_) {}
    setRaStatus(`Saved as ${user}`, 'ok');
});

raClearEl.addEventListener('click', () => {
    writeRaCredentials(null);
    renderRa();
});

// Close on Escape when the panel is open.
document.addEventListener('keydown', (e) => {
    if (isOpen && e.key === 'Escape') close();
});

// Keep identity + RA credentials in sync if the user is looking at the
// panel and identity arrives late (credentials are keyed by sub).
subscribe(() => {
    if (!isOpen) return;
    renderIdentity();
    renderRa();
});

export const userSettings = {
    open,
    close,
    toggle,
    get isOpen() { return isOpen; },
    getRaCredentials: readRaCredentials,
};
