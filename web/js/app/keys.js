// Keyboard, axis, and trigger dispatch — the pre-state handlers that
// translate raw KEY_PRESSED / AXIS_CHANGED events into state-machine
// callbacks, plus the help overlay toggle and the dpad-toggle UI
// helper. Owns the keyButtons DOM map too; anyone wiring UI to a
// specific key imports from here.

import {gui} from 'gui';
import {KEY, input} from 'input';
import {DPAD_TOGGLE, HELP_OVERLAY_TOGGLED, pub} from 'event';

import {stream} from '../stream.js?v=__V__';
import {screen} from '../screen.js?v=__V__';

import {store, setState} from './state.js?v=__V__';
import {app} from './lifecycle.js?v=__V__';

export const keyButtons = {};
Object.keys(KEY).forEach(button => {
    keyButtons[KEY[button]] = document.getElementById(`btn-${KEY[button]}`);
});

const helpOverlay = document.getElementById('help-overlay');

export const helpScreen = {
    shown: false,
    show(show, ev) {
        if (this.shown === show) return;

        const isGameScreen = store.state === app.state.game;
        screen.toggle(undefined, !show);

        gui.toggle(keyButtons[KEY.SAVE], show || isGameScreen);
        gui.toggle(keyButtons[KEY.LOAD], show || isGameScreen);
        gui.toggle(helpOverlay, show);

        this.shown = show;

        if (ev) pub(HELP_OVERLAY_TOGGLED, {shown: show});
    },
};

const _dpadArrowKeys = [KEY.UP, KEY.DOWN, KEY.LEFT, KEY.RIGHT];

export const onKeyPress = (data) => {
    const button = keyButtons[data.key];

    if (_dpadArrowKeys.includes(data.key)) {
        button && button.classList.add('dpad-pressed');
    } else {
        if (button) button.classList.add('pressed');
    }

    if (store.state !== app.state.settings) {
        if (KEY.HELP === data.key) helpScreen.show(true, event);
    }

    store.state.keyPress(data.key, data.code);
};

export const onKeyRelease = (data) => {
    const button = keyButtons[data.key];

    if (_dpadArrowKeys.includes(data.key)) {
        button && button.classList.remove('dpad-pressed');
    } else {
        if (button) button.classList.remove('pressed');
    }

    if (store.state !== app.state.settings) {
        if (KEY.HELP === data.key) helpScreen.show(false, event);
    }

    if (!store.interacted) {
        stream.audio.mute(false);
        store.interacted = true;
    }

    if (KEY.SETTINGS === data.key) setState(app.state.settings);

    store.state.keyRelease(data.key, data.code);
};

export const onAxisChanged = (data) => {
    if (!store.interacted) {
        stream.audio.mute(false);
        store.interacted = true;
    }

    store.state.axisChanged(data.id, data.value);
};

export const onTriggerChanged = (data) => {
    input.retropad.setTriggerChanged(data.id, data.value);
};

export const handleToggle = (force = false) => {
    const toggle = document.getElementById('dpad-toggle');

    force && toggle.setAttribute('checked', '');
    toggle.checked = !toggle.checked;
    pub(DPAD_TOGGLE, {checked: toggle.checked});
};
