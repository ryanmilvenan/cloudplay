import {
    pub,
    AXIS_CHANGED,
    GAMEPAD_CONNECTED,
    GAMEPAD_DISCONNECTED,
    KEY_PRESSED,
    KEY_RELEASED,
    TRIGGER_CHANGED,
} from 'event';
import {KEY} from 'input';
import {getControllerMap} from './controllerMaps.js?v=__V__';
import {log} from 'log';

const deadZone = 0.1;
let controllerMap = getControllerMap('');
let joystickMap = controllerMap.buttons;
let joystickState = {};
let joystickAxes = [];
let joystickTriggers = [0, 0];
let joystickIdx;
let joystickTimer = null;
let currentSystem = '';

function releaseDpadState() {
    checkJoystickAxisState(KEY.LEFT, false);
    checkJoystickAxisState(KEY.RIGHT, false);
    checkJoystickAxisState(KEY.UP, false);
    checkJoystickAxisState(KEY.DOWN, false);
}

function applyControllerMap(system = '') {
    currentSystem = system;
    controllerMap = getControllerMap(currentSystem);
    joystickMap = controllerMap.buttons;
    releaseDpadState();
}

// check state for each axis -> dpad
function checkJoystickAxisState(name, state) {
    if (joystickState[name] !== state) {
        joystickState[name] = state;
        pub(state === true ? KEY_PRESSED : KEY_RELEASED, {key: name});
    }
}

function checkJoystickAxis(axis, value) {
    if (-deadZone < value && value < deadZone) value = 0;
    if (joystickAxes[axis] !== value) {
        joystickAxes[axis] = value;
        pub(AXIS_CHANGED, {id: axis, value: value});
    }
}

function checkJoystickTrigger(idx, value) {
    if (joystickTriggers[idx] !== value) {
        joystickTriggers[idx] = value;
        pub(TRIGGER_CHANGED, {id: idx, value});
    }
}

// loop timer for checking joystick state
function checkJoystickState() {
    let gamepad = navigator.getGamepads()[joystickIdx];
    if (gamepad) {
        if (controllerMap.analogAxes) {
            gamepad.axes.forEach(function (value, index) {
                checkJoystickAxis(index, value);
            });
            releaseDpadState();
        } else {
            // D-pad-only platforms: only use the left stick as digital directions.
            let corX = gamepad.axes[0] || 0; // -1 -> 1, left -> right
            let corY = gamepad.axes[1] || 0; // -1 -> 1, up -> down
            checkJoystickAxisState(KEY.LEFT, corX <= -0.5);
            checkJoystickAxisState(KEY.RIGHT, corX >= 0.5);
            checkJoystickAxisState(KEY.UP, corY <= -0.5);
            checkJoystickAxisState(KEY.DOWN, corY >= 0.5);
        }

        if (controllerMap.analogTriggers && gamepad.buttons.length > 7) {
            const lt = gamepad.buttons[6];
            const rt = gamepad.buttons[7];
            checkJoystickTrigger(0, lt ? (lt.value || 0) : 0);
            checkJoystickTrigger(1, rt ? (rt.value || 0) : 0);
        } else {
            checkJoystickTrigger(0, 0);
            checkJoystickTrigger(1, 0);
        }

        // normal button map
        Object.keys(joystickMap).forEach(function (btnIdx) {
            const buttonState = gamepad.buttons[btnIdx];

            const isPressed = navigator.webkitGetGamepads ? buttonState === 1 :
                buttonState.value > 0 || buttonState.pressed === true;

            if (joystickState[btnIdx] !== isPressed) {
                joystickState[btnIdx] = isPressed;
                pub(isPressed === true ? KEY_PRESSED : KEY_RELEASED, {key: joystickMap[btnIdx]});
            }
        });
    }
}

// we only capture the last plugged joystick
const onGamepadConnected = (e) => {
    let gamepad = e.gamepad;
    log.info(`Gamepad connected at index ${gamepad.index}: ${gamepad.id}. ${gamepad.buttons.length} buttons, ${gamepad.axes.length} axes.`);
    console.log('[joystick] CONNECTED idx=' + gamepad.index + ' id="' + gamepad.id + '" buttons=' + gamepad.buttons.length + ' axes=' + gamepad.axes.length + ' mapping="' + gamepad.mapping + '"');

    joystickIdx = gamepad.index;

    applyControllerMap(currentSystem);

    // reset state
    joystickState = {[KEY.LEFT]: false, [KEY.RIGHT]: false, [KEY.UP]: false, [KEY.DOWN]: false};
    Object.keys(joystickMap).forEach(function (btnIdx) {
        joystickState[btnIdx] = false;
    });

    joystickAxes = new Array(Math.max(4, gamepad.axes.length)).fill(0);
    joystickTriggers = [0, 0];

    if (joystickTimer !== null) {
        clearInterval(joystickTimer);
    }

    joystickTimer = setInterval(checkJoystickState, 10);
    pub(GAMEPAD_CONNECTED);
};

/**
 * Joystick controls.
 *
 * cross == a      <--> a
 * circle == b     <--> b
 * square == x     <--> start
 * triangle == y   <--> select
 * share           <--> load
 * option          <--> save
 * L2 == LT        <--> full
 * R2 == RT        <--> quit
 * dpad            <--> up down left right
 * axis 0, 1       <--> second dpad
 *
 * change full to help (temporary)
 *
 * @version 1
 */
export const joystick = {
    setSystem: (system) => {
        applyControllerMap(system || '');

        // If a controller is already connected when the game/system changes,
        // immediately refresh the active mapping instead of waiting for a
        // disconnect/reconnect cycle.
        joystickState = {[KEY.LEFT]: false, [KEY.RIGHT]: false, [KEY.UP]: false, [KEY.DOWN]: false};
        Object.keys(joystickMap).forEach(function (btnIdx) {
            joystickState[btnIdx] = false;
        });
        joystickTriggers = [0, 0];

        const active = joystickIdx !== undefined && navigator.getGamepads && navigator.getGamepads()[joystickIdx];
        if (active) {
            joystickAxes = new Array(Math.max(4, active.axes.length)).fill(0);
        }

        log.info('[input] controller map set for system: ' + (system || 'default'));
    },
    init: () => {
        // we only capture the last plugged joystick
        window.addEventListener('gamepadconnected', onGamepadConnected);

        // disconnected event is triggered
        window.addEventListener('gamepaddisconnected', (event) => {
            clearInterval(joystickTimer);
            log.info(`Gamepad disconnected at index ${event.gamepad.index}`);
            pub(GAMEPAD_DISCONNECTED);
        });

        // Important: browsers often do not re-fire gamepadconnected for a pad
        // that was already connected before this page loaded. Detect those pads
        // on init so controller polling actually starts.
        if (navigator.getGamepads) {
            const existing = Array.from(navigator.getGamepads()).filter(Boolean);
            if (existing.length > 0) {
                onGamepadConnected({gamepad: existing[existing.length - 1]});
            }
        }

        log.info('[input] joystick has been initialized');
    }
}
