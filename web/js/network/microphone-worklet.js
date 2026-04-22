// AudioWorklet that decimates the AudioContext's native-rate float32 mono
// mix down to S16LE at a target rate (default 11025 Hz — the Dreamcast
// microphone's native sample rate) and posts packed Int16Array chunks
// back to the main thread for WebRTC data-channel transport.
//
// Decimation is naive (nearest-sample), not band-limited. That's fine
// for Seaman's speech-rate commands; if a future title needs better
// fidelity, drop in a FIR low-pass ahead of the decimation step.

class MicProcessor extends AudioWorkletProcessor {
    constructor(options) {
        super();
        const procOpts = (options && options.processorOptions) || {};
        this.targetRate = procOpts.targetRate || 11025;
        this.chunkSamples = procOpts.chunkSamples || 220; // ~20 ms @ 11025 Hz
        this.ratio = sampleRate / this.targetRate;
        this.acc = 0;
        this.buffer = new Int16Array(this.chunkSamples);
        this.filled = 0;
    }

    flushIfFull() {
        if (this.filled < this.chunkSamples) return;
        // Copy because postMessage transfer invalidates the underlying buffer
        // and we reuse our accumulator.
        const out = new Int16Array(this.chunkSamples);
        out.set(this.buffer);
        this.port.postMessage(out.buffer, [out.buffer]);
        this.filled = 0;
    }

    process(inputs) {
        const input = inputs[0];
        if (!input || input.length === 0) return true;
        const channel = input[0]; // mono mix from getUserMedia
        if (!channel) return true;

        for (let i = 0; i < channel.length; i++) {
            this.acc += 1;
            if (this.acc >= this.ratio) {
                this.acc -= this.ratio;
                const s = Math.max(-1, Math.min(1, channel[i]));
                const i16 = s < 0 ? (s * 0x8000) | 0 : (s * 0x7FFF) | 0;
                this.buffer[this.filled++] = i16;
                if (this.filled >= this.chunkSamples) {
                    this.flushIfFull();
                }
            }
        }
        return true;
    }
}

registerProcessor('mic-processor', MicProcessor);
