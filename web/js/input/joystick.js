import {
    pub,
    sub,
    AXIS_CHANGED,
    DPAD_TOGGLE,
    GAMEPAD_CONNECTED,
    GAMEPAD_DISCONNECTED,
    KEY_PRESSED,
    KEY_RELEASED
} from 'event';
import {KEY} from 'input';
import {getControllerMap} from './controllerMaps.js?v=5';
import {log} from 'log';

const deadZone = 0.1;
let joystickMap;
let joystickState = {};
let joystickAxes = [];
let joystickIdx;
let joystickTimer = null;
let dpadMode = true;
let currentSystem = '';

function onDpadToggle(checked) {
    if (dpadMode === checked) {
        return //error?
    }
    if (dpadMode) {
        dpadMode = false;
        // reset dpad keys pressed before moving to analog stick mode
        checkJoystickAxisState(KEY.LEFT, false);
        checkJoystickAxisState(KEY.RIGHT, false);
        checkJoystickAxisState(KEY.UP, false);
        checkJoystickAxisState(KEY.DOWN, false);
    } else {
        dpadMode = true;
        // reset analog stick axes before moving to dpad mode
        joystickAxes.forEach(function (value, index) {
            checkJoystickAxis(index, 0);
        });
    }
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

// loop timer for checking joystick state
function checkJoystickState() {
    let gamepad = navigator.getGamepads()[joystickIdx];
    if (gamepad) {
        if (dpadMode) {
            // axis -> dpad
            let corX = gamepad.axes[0]; // -1 -> 1, left -> right
            let corY = gamepad.axes[1]; // -1 -> 1, up -> down
            checkJoystickAxisState(KEY.LEFT, corX <= -0.5);
            checkJoystickAxisState(KEY.RIGHT, corX >= 0.5);
            checkJoystickAxisState(KEY.UP, corY <= -0.5);
            checkJoystickAxisState(KEY.DOWN, corY >= 0.5);
        } else {
            gamepad.axes.forEach(function (value, index) {
                checkJoystickAxis(index, value);
            });
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

    joystickIdx = gamepad.index;

    // Load controller map for the current system
    const map = getControllerMap(currentSystem);
    joystickMap = map.buttons;
    dpadMode = map.dpadMode;

    // reset state
    joystickState = {[KEY.LEFT]: false, [KEY.RIGHT]: false, [KEY.UP]: false, [KEY.DOWN]: false};
    Object.keys(joystickMap).forEach(function (btnIdx) {
        joystickState[btnIdx] = false;
    });

    joystickAxes = new Array(gamepad.axes.length).fill(0);

    // looper, too intense?
    if (joystickTimer !== null) {
        clearInterval(joystickTimer);
    }

    joystickTimer = setInterval(checkJoystickState, 10); // milliseconds per hit
    pub(GAMEPAD_CONNECTED);
};

sub(DPAD_TOGGLE, (data) => onDpadToggle(data.checked));

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
        currentSystem = system || '';
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

        log.info('[input] joystick has been initialized');
    }
}
