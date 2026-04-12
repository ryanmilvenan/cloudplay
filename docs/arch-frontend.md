# Frontend architecture

> Update this diagram when you change how the frontend is structured.
> See [../CLAUDE.md](../CLAUDE.md) for what counts as "structural".

Entry: `web/index.html` → ESM import map → `web/js/app.js`.
Served from moon's `~/containers/cloudretro-phase3/web/` (bind-mounted, read-only into container). Rsync-deployed via `/deploy-cloudplay-frontend`; no rebuild.

```mermaid
flowchart TB
    index["web/index.html<br/>importmap"]
    appjs["app.js<br/>(composition root, ~45 lines)"]

    subgraph appmod["web/js/app/"]
        lifecycle["lifecycle.js<br/>state machine<br/>(eden · settings · menu · game)"]
        session["session.js<br/>startGame · slot picker<br/>overlay callbacks"]
        keys["keys.js<br/>onKeyPress/Release<br/>axis · trigger · dpad"]
        wiring["wiring.js<br/>every sub() lives here<br/>onMessage"]
        rumble["rumble.js"]
        stats["statsProbes.js"]
        orphan["orphanRecover.js"]
    end

    state["state.js<br/>getState · setState(patch) · subscribe"]
    event["event.js<br/>pub · sub · typed events"]

    subgraph ui["UI modules (DOM owners)"]
        overlay["overlay.js<br/>subscribes to state"]
        gameList["gameList.js"]
        menu["menu.js"]
        stream["stream.js"]
        screen["screen.js"]
        settings["settings.js"]
        message["message.js"]
        statsUI["stats.js"]
        gui["gui.js"]
    end

    subgraph input["input/"]
        joystick["joystick.js"]
        keyboard["keyboard.js"]
        touch["touch.js"]
        pointer["pointer.js"]
        retropad["retropad.js"]
    end

    subgraph network["network/"]
        socket["socket.js"]
        webrtc["webrtc.js"]
    end

    api["api.js"]
    room["room.js"]
    workerManager["workerManager.js"]
    recording["recording.js"]

    index --> appjs

    appjs --> lifecycle
    appjs --> wiring
    appjs --> stats
    appjs --> orphan
    appjs --> state

    lifecycle -.uses.-> session
    lifecycle -.uses.-> keys
    session -.uses.-> lifecycle
    keys -.uses.-> lifecycle
    session -.uses.-> state
    keys -.uses.-> state
    lifecycle -.uses.-> state
    wiring -.uses.-> state
    wiring -.uses.-> session
    wiring -.uses.-> keys
    wiring -.uses.-> lifecycle
    wiring -.uses.-> rumble

    wiring -- pub/sub --> event
    input -- pub KEY/AXIS --> event
    webrtc -- pub WEBRTC_* --> event
    socket -- pub MESSAGE --> event

    state -- notify --> overlay

    session -- setState --> state
    wiring -- setState<br/>(ROOM_MEMBERS) --> state

    wiring -.reads.-> webrtc
    wiring -.reads.-> socket
    appjs --> socket

    session -.-> api
    session -.-> overlay
    session -.-> gameList
    session -.-> screen
    session -.-> stream
    session -.-> message
    session -.-> room
    session -.-> workerManager
    session -.-> recording
    lifecycle -.-> overlay
    lifecycle -.-> gameList
    lifecycle -.-> menu
    lifecycle -.-> screen
    lifecycle -.-> stream
    lifecycle -.-> statsUI

    classDef shim fill:#fff3cd,stroke:#664d00;
    classDef new fill:#d4edda,stroke:#155724;
    class state,appmod new;
```

## Data-flow quick reference

```
Server ── msg ──▶ socket ── pub MESSAGE ──▶ wiring.onMessage
                                                   │
                                                   ├──▶ pub(WEBRTC_*)    ── webrtc consumes
                                                   ├──▶ pub(GAME_*)      ── wiring subs
                                                   └──▶ setState(...)    ── state.js
                                                                               │ notify
                                                                               ▼
                                                                          subscribers
                                                                          (overlay …)
```

## Notable invariants

- **Every `sub()` call lives in `wiring.js`** (except a small set of module-local subs in `screen.js`, `stream.js`, `keyboard.js`, `touch.js`, `settings.js`, `workerManager.js`, `statsProbes.js`). New subs should default to wiring.
- **`setState(patch)` and `setAppState(...)` are the only sanctioned writers** of cross-module state. Direct mutation of `getState()` is a bug.
- **Circular imports** between `lifecycle ↔ session ↔ keys` are load-safe because every circular reference is called at runtime, not at module init. Adding a top-level expression that evaluates one of these bindings immediately is a footgun — put it inside a function.
- **`?v=__V__` placeholder** in every import gets stamped by `scripts/version.sh` at deploy time (`/deploy-cloudplay-frontend`). No per-file manual bumps.
