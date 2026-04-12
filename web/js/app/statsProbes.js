import {webrtc} from 'network';
import {APP_VIDEO_CHANGED, sub} from 'event';

import {stats} from '../stats.js?v=__V__';

let WEBRTC_STATS_RTT;
let VIDEO_BITRATE;
let GET_V_CODEC, SET_CODEC;

// Rolling mean over a 6-sample window of instantaneous bitrate (bits/s).
// Returns a function that takes (now, bytes) and returns kb/s.
const createBitrateSmoother = (windowSize = 6) => {
    const window = Array(windowSize).fill(0);
    let prevBytes, prevTime, idx = 0;
    return (now, bytes) => {
        window[idx++ % windowSize] = prevTime
            ? Math.floor(8 * (bytes - prevBytes) / (now - prevTime))
            : 0;
        prevBytes = bytes;
        prevTime = now;
        return Math.floor(window.reduce((a, b) => a + b) / windowSize);
    };
};

const bitrate = createBitrateSmoother();

export const initStatsProbes = () => {
    stats.modules = [
        {
            mui: stats.mui('', '<1'),
            init() {
                WEBRTC_STATS_RTT = (v) => (this.val = v);
            },
        },
        {
            mui: stats.mui('', '', false, () => ''),
            init() {
                GET_V_CODEC = (v) => (this.val = v + ' @ ');
            },
        },
        {
            mui: stats.mui('', '', false, () => ''),
            init() {
                sub(APP_VIDEO_CHANGED, ({s = 1, w, h}) => (this.val = `${w * s}x${h * s}`));
            },
        },
        {
            mui: stats.mui('', '', false, () => ' kb/s', 'stats-bitrate'),
            init() {
                VIDEO_BITRATE = (v) => (this.val = v);
            },
        },
        {
            async stats() {
                const stats = await webrtc.stats();
                if (!stats) return;

                stats.forEach(report => {
                    if (!SET_CODEC && report.mimeType?.startsWith('video/')) {
                        GET_V_CODEC(report.mimeType.replace('video/', '').toLowerCase());
                        SET_CODEC = 1;
                    }
                    const {nominated, currentRoundTripTime, type, kind} = report;
                    if (nominated && currentRoundTripTime !== undefined) {
                        WEBRTC_STATS_RTT(currentRoundTripTime * 1000);
                    }
                    if (type === 'inbound-rtp' && kind === 'video') {
                        VIDEO_BITRATE(bitrate(report.timestamp, report.bytesReceived));
                    }
                });
            },
            enable() {
                this.interval = window.setInterval(this.stats, 999);
            },
            disable() {
                window.clearInterval(this.interval);
            },
        },
    ];

    stats.toggle();
};
