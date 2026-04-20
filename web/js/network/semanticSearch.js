// Phase-3 semantic-search client. Talks to the worker's
// /v1/search/semantic endpoint; the browser already connects to the
// worker for WebRTC pings so the origin + CORS story is already sorted.
//
// Public surface:
//   semanticSearch(query, top?) → Promise<Hit[]>     // Hit = {game_path, system, score}
//   isAvailable() → boolean                          // feature-flag check, reads a module-level
//                                                    //   cache set by the first probe
//
// Failure mode is always "return []" — the fuzzy path owns the baseline
// so an unreachable embedder, a disabled service, or a slow response
// shouldn't block the UI. See app/wiring.js for how results are
// merged into the fuzzy list.

import {log} from 'log';

let available = null; // null = unknown, true/false after first probe
let endpointURL = null;

// Resolve the worker's HTTP endpoint. Worker addr lives on
// window.location (same host cloudplay serves from) for the default
// single-container deploy; the singlePort reverse-proxy on the
// coordinator handles routing inside the box.
const resolveEndpoint = () => {
    if (endpointURL) return endpointURL;
    const origin = window.location.origin;
    endpointURL = origin.replace(/\/$/, '') + '/v1/search/semantic';
    return endpointURL;
};

export const isAvailable = () => available === true;

// probe() hits the endpoint with an empty query once; a 2xx means the
// worker has the feature wired, any other status or network error
// means fuzzy-only. Called lazily by semanticSearch on first use.
const probe = async () => {
    try {
        const r = await fetch(resolveEndpoint() + '?q=&top=1', {
            method: 'GET',
            headers: {'Accept': 'application/json'},
        });
        available = r.ok;
    } catch (e) {
        available = false;
        log.debug?.('[semantic] probe failed', e);
    }
};

export const semanticSearch = async (query, top = 10) => {
    if (available === null) await probe();
    if (!available) return [];
    const q = (query || '').trim();
    if (!q) return [];
    try {
        const r = await fetch(resolveEndpoint(), {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({q, top}),
        });
        if (!r.ok) return [];
        const body = await r.json();
        return Array.isArray(body?.hits) ? body.hits : [];
    } catch (e) {
        log.debug?.('[semantic] fetch failed', e);
        return [];
    }
};
