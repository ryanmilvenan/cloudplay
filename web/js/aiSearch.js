// AI-search cinematic UI: the grayed turn chain above the search bar
// and the single-line "breath" response below it. Owned by this module;
// exposes ask(query), toggle, and mode getters for wiring.js.
//
// Visual model (per product direction):
//   - Chain is a light gradient-faded list of past human + AI turns,
//     max 4 visible. Items older than 4 fall off entirely.
//   - Breath is ONE line. It fades in on first token (Phase 4) or on
//     response arrival (Phase 1 stub). It stays visible until the
//     user's next submission, at which point the previous breath fades
//     out and the chain receives it.
//   - Entire flow is cinematic: no chat window, no bubbles, no
//     send-button. Just a bar, a whisper, a memory.
//
// Phase 1 ships the UI shell with a stubbed ask() that echoes a fixed
// placeholder so we can validate the animation choreography before
// Phase 4 wires the live LLM endpoint.

const MAX_CHAIN = 4;

const chainEl = () => document.getElementById('ai-chain');
const breathEl = () => document.getElementById('ai-breath');
const aiToggleEl = () => document.getElementById('game-select-ai-toggle');
const inputEl = () => document.getElementById('game-select-input');

// AI toggle — click switches mode. Persisted to localStorage so a page
// refresh remembers the user's choice.
const AI_MODE_KEY = 'cloudplay.aiMode';
let aiMode = (() => {
    const raw = localStorage.getItem(AI_MODE_KEY);
    return raw === null ? true : raw === 'true';
})();

const applyModeVisual = () => {
    const btn = aiToggleEl();
    if (!btn) return;
    btn.classList.toggle('is-on', aiMode);
    btn.setAttribute('aria-pressed', aiMode ? 'true' : 'false');
    btn.title = aiMode
        ? 'AI mode — natural-language queries'
        : 'Fuzzy mode — direct title search';
};

export const setMode = (on) => {
    aiMode = !!on;
    localStorage.setItem(AI_MODE_KEY, String(aiMode));
    applyModeVisual();
};
export const getMode = () => aiMode;
export const toggle = () => setMode(!aiMode);

// Turn chain state. Each entry is {role:'user'|'ai', text}.
// index 0 = most recent; we render in reverse so the newest line sits
// closest to the search bar and older ones fade toward the top.
const turns = [];

const renderChain = () => {
    const root = chainEl();
    if (!root) return;
    root.innerHTML = '';
    // Render top-to-bottom with newest first — most recent turn sits
    // directly beneath the search bar, older turns sink toward the
    // floor of the viewport and dim as they go. This matches a
    // "recent memories fade into the past" model rather than a
    // traditional chat log.
    const visible = turns.slice(0, MAX_CHAIN);
    visible.forEach((turn, i) => {
        const el = document.createElement('div');
        // Age → opacity. i=0 is newest at 0.72; each step down sheds
        // ~0.2 until anything past index 3 would vanish entirely.
        const opacity = Math.max(0.1, 0.72 - i * 0.2);
        el.className = 'ai-chain__entry ai-chain__entry--' + turn.role;
        el.style.opacity = String(opacity);
        el.textContent = turn.text;
        root.appendChild(el);
    });
};

const pushTurn = (role, text) => {
    turns.unshift({ role, text });
    while (turns.length > MAX_CHAIN) turns.pop();
    renderChain();
};

// breath: the hero line. Shows a single message with a fade-in entry.
// Passing null clears it (fade out).
// showBreath(text) — slow emergence from darkness. Fade-in is ~2s
// with a shallow ease (slow start, gradual reveal). Fade-out is ~1.2s
// so a turn transition feels like one long inhale/exhale, not a cut.
// The text-clear happens only after the fade-out completes so we
// never snap mid-animation.
const FADE_OUT_MS = 1200;
const showBreath = (text) => {
    const el = breathEl();
    if (!el) return;
    if (!text) {
        el.classList.remove('is-visible');
        setTimeout(() => {
            if (!el.classList.contains('is-visible')) el.textContent = '';
        }, FADE_OUT_MS);
        return;
    }
    // Retrigger transition even if already visible — fade to zero
    // briefly, then fade back in with the new text. Matches the
    // "voice of god taking a breath between thoughts" feel.
    el.classList.remove('is-visible');
    el.textContent = text;
    // Force reflow so the class-add actually re-runs the transition.
    void el.offsetWidth;
    el.classList.add('is-visible');
};

// liveAi: the AI response currently on-screen in the breath. It lives
// here *instead of* the chain until the user submits the next turn,
// then it's moved into the chain as the breath fades out for the new
// response. This is what keeps the breath/chain from visually
// double-printing the same line.
let liveAi = null;

// onLaunch: set by wiring.js so the agent's `launch` action can hand
// back to the normal start-game flow. Set after init().
let onLaunch = null;
export const setLaunchHandler = (fn) => { onLaunch = fn; };

// Conversation history mirrors what the backend agent needs in its
// prompt. Trimmed to the last N turns so the prompt stays cheap; the
// gradient chain in the UI shows the most recent four.
const MAX_HISTORY = 8;
const history = []; // [{role:'user'|'agent', text}]

// ask(query): Phase 4 — POST to /v1/agent/turn, interpret the
// {action, text, ...} response, and animate accordingly. The
// "voice of god" breath animation from Phase 1 is preserved; the
// only change is the text source (the LLM instead of a stub).
export const ask = async (query) => {
    if (!query || !query.trim()) return;
    // Previous AI response graduates from breath → chain.
    if (liveAi) {
        pushTurn('ai', liveAi);
        liveAi = null;
    }
    pushTurn('user', query);
    history.push({role: 'user', text: query});
    trimHistory();
    // Fade out the previous breath before the new one surfaces.
    showBreath(null);
    // "Thinking" gap — covers network + LLM latency with the same
    // quiet inhale-between-turns feel as a no-op transition.
    await wait(600);

    let action;
    try {
        const resp = await fetch('/v1/agent/turn', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({query, history: history.slice(0, -1)}),
        });
        action = await resp.json();
    } catch (e) {
        action = {action: 'say', text: 'I couldn\u2019t reach the assistant — try picking from the list.'};
    }

    const text = (action && action.text) ? String(action.text) : '';
    if (text) {
        liveAi = text;
        showBreath(text);
        history.push({role: 'agent', text});
        trimHistory();
    }

    // Launch action: after the breath shows briefly, kick the normal
    // start-game flow. The breath stays visible across the transition
    // — the game view replaces the menu so the overlay disappears
    // naturally when the stream takes over.
    if (action && action.action === 'launch' && action.game_path && action.system) {
        if (typeof onLaunch === 'function') {
            setTimeout(() => onLaunch(action), 400);
        }
    }
};

const trimHistory = () => {
    while (history.length > MAX_HISTORY) history.shift();
};

const wait = (ms) => new Promise(r => setTimeout(r, ms));

// Clear the whole AI flow — called when the user picks a fuzzy result
// (no pending clarifying conversation) or when a game actually launches.
export const clearConversation = () => {
    turns.length = 0;
    liveAi = null;
    renderChain();
    showBreath(null);
};

// onUserTyping(): called from gameList's input handler the instant the
// user starts editing the bar again. Dismisses the big top-third breath
// so it doesn't linger beside fresh fuzzy results, and graduates the
// current live AI response into the decaying chain below the bar so
// the line isn't lost — it just moves to where recent memories live.
export const onUserTyping = () => {
    const el = breathEl();
    if (!el || !el.classList.contains('is-visible')) return;
    if (liveAi) {
        pushTurn('ai', liveAi);
        liveAi = null;
    }
    showBreath(null);
};

// Bind toggle button click handler. Idempotent.
let bound = false;
export const init = () => {
    if (bound) return;
    bound = true;
    applyModeVisual();
    const btn = aiToggleEl();
    if (btn) {
        btn.addEventListener('click', () => {
            toggle();
            const inp = inputEl();
            if (!inp) return;
            // Refocus the input so the user can keep typing without a click.
            inp.focus();
            // Re-fire input so gameList.applyQuery re-evaluates under
            // the new mode — toggling from AI→fuzzy with text in the
            // bar should show fuzzy matches immediately; fuzzy→AI should
            // clear the dropdown.
            inp.dispatchEvent(new Event('input', {bubbles: true}));
        });
    }
};
