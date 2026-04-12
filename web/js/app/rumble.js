// Rumble/haptic feedback via the Gamepad Vibration API.
//
// The worker sends a 5-byte binary frame per motor update:
// [0xFF, port, effect, strength_hi, strength_lo]. Because each
// playEffect call replaces the previous one, we track both motors
// per port and debounce briefly so a strong+weak pair arriving
// within the same tick becomes a single combined vibration.

const rumbleState = [
    {strong: 0, weak: 0, timer: null},
    {strong: 0, weak: 0, timer: null},
    {strong: 0, weak: 0, timer: null},
    {strong: 0, weak: 0, timer: null},
];

const applyRumble = (port) => {
    const gamepads = navigator.getGamepads ? navigator.getGamepads() : [];
    const gp = gamepads[port];
    if (!gp || !gp.vibrationActuator) return;
    const s = rumbleState[port];
    gp.vibrationActuator.playEffect('dual-rumble', {
        duration: 150,
        strongMagnitude: s.strong,
        weakMagnitude: s.weak,
    }).catch(() => {});
};

export const handleRumble = (port, effect, strength) => {
    if (port >= rumbleState.length) return;
    const s = rumbleState[port];
    const intensity = strength / 0xFFFF;
    if (effect === 0) s.strong = intensity;
    else s.weak = intensity;
    if (s.timer) clearTimeout(s.timer);
    s.timer = setTimeout(() => { s.timer = null; applyRumble(port); }, 5);
};
