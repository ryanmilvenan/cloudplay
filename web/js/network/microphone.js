// Microphone uplink. Opt-in via the "Enable microphone" setting (see
// settings.js). Browser records mic audio with getUserMedia, an
// AudioWorklet (microphone-worklet.js) resamples the AudioContext's
// native rate to 11025 Hz mono S16LE — the Dreamcast mic's native rate —
// and chunks land on the WebRTC "microphone" data channel, which the
// worker forwards to the active adapter's Input(port, device=3, pcm).
// Only native-process adapters (flycast) actually deliver it into the
// emulator; other adapters no-op on device 3.

import {log} from 'log';
import {webrtc} from 'network';
import {opts, settings} from '../settings.js?v=__V__';

let stream = null;
let audioContext = null;
let workletNode = null;
let sourceNode = null;
let active = false;

const TARGET_RATE = 11025;

// init seeds the setting store so the cog-menu settings panel's render()
// sees the ENABLE_MICROPHONE key and draws a row for it. Must be called
// AFTER settings.init() (which wires up the localStorage provider), so
// this is an explicit function rather than module-top-level code — the
// same pattern screen.init / input.init / stream.init follow. app.js
// calls it during bootstrap after settings.init().
const init = () => {
    settings.loadOr(opts.ENABLE_MICROPHONE, false);
};

const isEnabled = () => !!settings.loadOr(opts.ENABLE_MICROPHONE, false);

const start = async () => {
    log.info('[mic] start() called active=' + active + ' enabled=' + isEnabled());
    if (active) return;
    if (!isEnabled()) {
        log.info('[mic] skipped — ENABLE_MICROPHONE setting is off');
        return;
    }
    if (!navigator.mediaDevices || !window.AudioWorkletNode) {
        log.warn('[mic] browser lacks getUserMedia/AudioWorklet; mic uplink disabled');
        return;
    }
    log.info('[mic] requesting getUserMedia permission…');
    try {
        // Disable the aggressive defaults — the Dreamcast speech recognizer
        // wants raw sound, not Chrome's voice-chat-tuned processing.
        stream = await navigator.mediaDevices.getUserMedia({
            audio: {
                echoCancellation: false,
                noiseSuppression: false,
                autoGainControl: false,
            },
        });
        log.info('[mic] permission granted, stream acquired');
    } catch (e) {
        log.warn('[mic] getUserMedia denied or failed:', e);
        return;
    }
    try {
        audioContext = new (window.AudioContext || window.webkitAudioContext)();
        await audioContext.audioWorklet.addModule('js/network/microphone-worklet.js?v=__V__');
        sourceNode = audioContext.createMediaStreamSource(stream);
        workletNode = new AudioWorkletNode(audioContext, 'mic-processor', {
            processorOptions: {targetRate: TARGET_RATE},
        });
        workletNode.port.onmessage = (e) => {
            // ArrayBuffer of Int16 samples, already in s16le order on LE
            // hosts (DataView-less write in the worklet relies on this).
            webrtc.mic(e.data);
        };
        sourceNode.connect(workletNode);
        // Worklet outputs silence on the audio graph — we only use it for
        // processing, not playback — but it still needs to reach an
        // endpoint to be scheduled. Wire through a muted GainNode rather
        // than the speakers to avoid self-hearing.
        const muteSink = audioContext.createGain();
        muteSink.gain.value = 0;
        workletNode.connect(muteSink).connect(audioContext.destination);
        active = true;
        log.info('[mic] capture started @', audioContext.sampleRate, 'Hz → ', TARGET_RATE, 'Hz');
    } catch (e) {
        log.error('[mic] worklet setup failed:', e);
        stop();
    }
};

const stop = () => {
    active = false;
    try { workletNode?.port.close?.(); } catch {}
    try { sourceNode?.disconnect(); } catch {}
    try { workletNode?.disconnect(); } catch {}
    try { audioContext?.close(); } catch {}
    if (stream) {
        stream.getTracks().forEach(t => { try { t.stop(); } catch {} });
    }
    stream = null;
    audioContext = null;
    workletNode = null;
    sourceNode = null;
};

export const mic = {
    init,
    start,
    stop,
    isActive: () => active,
};
