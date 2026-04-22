// Composition root: imports the app modules, bootstraps browser state,
// wires the event bus, and kicks off the connection flow. All behaviour
// lives in ./app/* — this file just stitches them together.

import {log} from 'log';
import {api} from 'api';
import {input} from 'input';
import {mic, socket, webrtc} from 'network';
import {opts, settings} from 'settings';

import {menu} from './menu.js?v=__V__';
import {room} from './room.js?v=__V__';
import {screen} from './screen.js?v=__V__';
import {stream} from './stream.js?v=__V__';

import {app, setAppState} from './app/lifecycle.js?v=__V__';
import {initWiring} from './app/wiring.js?v=__V__';
import './userSettings.js?v=__V__';
import {initStatsProbes} from './app/statsProbes.js?v=__V__';
import {initOrphanRecover} from './app/orphanRecover.js?v=__V__';

settings.init();
log.level = settings.loadOr(opts.LOG_LEVEL, log.DEFAULT);
mic.init();

screen.add(menu, stream);

initWiring();

setAppState(app.state.eden);

input.init();
stream.init();
screen.init();

const [roomId, zone] = room.loadMaybe();
const wid = new URLSearchParams(document.location.search).get('wid');
socket.init(roomId, wid, zone);
api.transport = {
    send: socket.send,
    keyboard: webrtc.keyboard,
    mouse: webrtc.mouse,
};

initStatsProbes();
initOrphanRecover();
