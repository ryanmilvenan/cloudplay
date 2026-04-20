import {
    pub,
    MESSAGE
} from 'event';
import {log} from 'log';

let conn;
// Remember the last init params so reopen() can reconnect without the
// caller having to thread them through again. Populated on every init().
let lastParams = null;

const buildUrl = (params = {}) => {
    const url = new URL(window.location);
    url.protocol = location.protocol !== 'https:' ? 'ws' : 'wss';
    url.pathname = "/ws";
    Object.keys(params).forEach(k => {
        if (!!params[k]) url.searchParams.set(k, params[k])
    })
    return url
}

const init = (roomId, wid, zone) => {
    lastParams = {roomId, wid, zone};
    let objParams = {room_id: roomId, zone: zone};
    if (wid) objParams.wid = wid;
    const url = buildUrl(objParams)
    log.info(`[ws] connecting to ${url}`);
    conn = new WebSocket(url.toString());
    conn.onopen = () => {
        log.info('[ws] <- open connection');
    };
    conn.onerror = () => log.error('[ws] some error!');
    conn.onclose = (event) => log.info(`[ws] closed (${event.code})`);
    conn.onmessage = response => {
        const data = JSON.parse(response.data);
        log.debug('[ws] <- ', data);
        pub(MESSAGE, data);
    };
};

const close = () => {
    if (conn && conn.readyState !== WebSocket.CLOSED) {
        try { conn.close(1000, 'client reset'); } catch (e) { /* ignore */ }
    }
    conn = null;
};

// reopen — used by the "Blow on the cartridge" reset path. Tears the
// existing socket down and re-initialises with the last parameters, so
// the coordinator issues a fresh InitSession and rebinds the user to
// whatever worker is currently healthy (instead of the dead one we may
// be pointing at).
const reopen = () => {
    const params = lastParams || {};
    close();
    init(params.roomId, params.wid, params.zone);
};

const send = (data) => {
    if (conn && conn.readyState === 1) {
        conn.send(JSON.stringify(data));
    }
}

/**
 * WebSocket connection module.
 *
 *  Needs init() call.
 */
export const socket = {
    init,
    send,
    close,
    reopen,
}
