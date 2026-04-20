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
const breathInfoEl = () => document.getElementById('ai-breath-info');
const aiToggleEl = () => document.getElementById('game-select-ai-toggle');
const inputEl = () => document.getElementById('game-select-input');
const panelEl = () => document.getElementById('ai-retrieved-panel');
const panelListEl = () => document.getElementById('ai-retrieved-list');
const panelCloseEl = () => document.getElementById('ai-retrieved-close');
const panelBackdropEl = () => panelEl()?.querySelector('.ai-retrieved-panel__backdrop');

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

// Turn chain state. Each entry is {role:'user'|'ai', text, retrieved?}.
// index 0 = most recent; we render in reverse so the newest line sits
// closest to the search bar and older ones fade toward the top.
// retrieved (AI turns only) is the ranked candidate list the LLM saw
// — kept so the user can re-open it from the chain to debug or launch.
const turns = [];

const INFO_ICON_SVG = `<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8h.01"/><path d="M11 12h1v4h1"/></svg>`;

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
        const textNode = document.createElement('span');
        textNode.className = 'ai-chain__text';
        textNode.textContent = turn.text;
        el.appendChild(textNode);
        if (turn.role === 'ai' && Array.isArray(turn.retrieved) && turn.retrieved.length) {
            const btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'ai-chain__info';
            btn.title = `What the agent saw (${turn.retrieved.length})`;
            btn.setAttribute('aria-label', btn.title);
            btn.innerHTML = INFO_ICON_SVG;
            btn.addEventListener('click', (e) => {
                e.stopPropagation();
                openRetrievedPanel(turn.retrieved);
            });
            el.appendChild(btn);
        }
        root.appendChild(el);
    });
};

const pushTurn = (role, text, retrieved) => {
    const turn = { role, text };
    if (retrieved && retrieved.length) turn.retrieved = retrieved;
    turns.unshift(turn);
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
// liveRetrieved: the candidate set the LLM saw for the current breath.
// Moves into the chain entry alongside liveAi when the breath
// graduates. Drives the small info button anchored to the breath.
let liveRetrieved = null;

// Breath auto-fade. After the response arrives, hold the big
// "voice of god" line for BREATH_HOLD_MS, then gracefully graduate
// it into the chain so the cinematic moment doesn't linger forever.
// The user can always re-open the retrieved candidates via the info
// button on the chain entry, so nothing is lost.
const BREATH_HOLD_MS = 8000;
let breathHoldTimer = null;

const clearBreathHold = () => {
    if (breathHoldTimer) {
        clearTimeout(breathHoldTimer);
        breathHoldTimer = null;
    }
};

const graduateBreath = () => {
    clearBreathHold();
    if (liveAi) {
        pushTurn('ai', liveAi, liveRetrieved);
        liveAi = null;
        liveRetrieved = null;
    }
    showBreath(null);
    setBreathInfoVisible(false);
};

const setBreathInfoVisible = (show) => {
    const el = breathInfoEl();
    if (!el) return;
    if (show) {
        el.classList.remove('hidden');
        // next tick so the opacity transition runs
        requestAnimationFrame(() => el.classList.add('is-visible'));
    } else {
        el.classList.remove('is-visible');
        el.classList.add('hidden');
    }
};

const openRetrievedPanel = (retrieved) => {
    const panel = panelEl();
    const list = panelListEl();
    if (!panel || !list) return;
    list.innerHTML = '';
    if (!retrieved || !retrieved.length) {
        const empty = document.createElement('div');
        empty.className = 'ai-retrieved-panel__empty';
        empty.textContent = 'The agent had no candidates for that turn.';
        list.appendChild(empty);
    } else {
        retrieved.forEach((c, i) => {
            const row = document.createElement('div');
            row.className = 'ai-retrieved-panel__row';
            row.setAttribute('role', 'button');
            row.tabIndex = 0;
            const rank = typeof c.rank === 'number' && c.rank > 0 ? c.rank : i + 1;
            const coverHtml = c.cover_url
                ? `<img class="ai-retrieved-panel__cover" src="${escapeHtml(c.cover_url)}" alt="" loading="lazy" onerror="this.classList.add('ai-retrieved-panel__cover--empty');this.removeAttribute('src');">`
                : `<div class="ai-retrieved-panel__cover ai-retrieved-panel__cover--empty"></div>`;
            const metaBits = [];
            if (c.system) metaBits.push(`<span class="ai-retrieved-panel__meta-sys">${escapeHtml(c.system)}</span>`);
            if (c.genre) metaBits.push(escapeHtml(c.genre));
            if (c.year) metaBits.push(String(c.year));
            if (c.franchise && (!c.title || c.franchise.toLowerCase() !== c.title.toLowerCase())) {
                metaBits.push(`series: ${escapeHtml(c.franchise)}`);
            }
            row.innerHTML =
                `<div class="ai-retrieved-panel__rank">${rank}</div>` +
                coverHtml +
                `<div class="ai-retrieved-panel__body">` +
                  `<div class="ai-retrieved-panel__title-line">${escapeHtml(c.title || c.game_path || '(untitled)')}</div>` +
                  `<div class="ai-retrieved-panel__meta">${metaBits.join(' · ')}</div>` +
                `</div>`;
            const launch = () => {
                if (!c.game_path || !c.system) { closeRetrievedPanel(); return; }
                closeRetrievedPanel();
                if (typeof onLaunch === 'function') {
                    onLaunch({action: 'launch', game_path: c.game_path, system: c.system});
                }
            };
            row.addEventListener('click', launch);
            row.addEventListener('keydown', (e) => {
                if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); launch(); }
            });
            list.appendChild(row);
        });
    }
    panel.classList.remove('hidden');
};

const closeRetrievedPanel = () => {
    const panel = panelEl();
    if (!panel) return;
    panel.classList.add('hidden');
};

const escapeHtml = (s = '') =>
    String(s).replace(/[&<>"']/g, (c) => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    })[c]);

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
    // Previous AI response graduates from breath → chain, carrying
    // its retrieved set so the user can still open "what did the
    // agent see" on the older turn. Also cancels any pending
    // auto-fade timer — the new turn is taking over.
    clearBreathHold();
    if (liveAi) {
        pushTurn('ai', liveAi, liveRetrieved);
        liveAi = null;
        liveRetrieved = null;
    }
    pushTurn('user', query);
    history.push({role: 'user', text: query});
    trimHistory();
    // Fade out the previous breath + its info affordance before the
    // new response surfaces.
    showBreath(null);
    setBreathInfoVisible(false);
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
    const retrieved = (action && Array.isArray(action.retrieved)) ? action.retrieved : null;
    if (text) {
        liveAi = text;
        liveRetrieved = retrieved;
        showBreath(text);
        setBreathInfoVisible(!!(retrieved && retrieved.length));
        history.push({role: 'agent', text});
        trimHistory();
        // Start the auto-fade countdown. The response is already
        // recorded in `history` (for the next turn's prompt) and
        // about to land in the chain (where the info button still
        // exposes the retrieved set), so we don't need to hold the
        // voice-of-god line on-screen forever.
        clearBreathHold();
        breathHoldTimer = setTimeout(graduateBreath, BREATH_HOLD_MS);
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
    clearBreathHold();
    turns.length = 0;
    liveAi = null;
    liveRetrieved = null;
    renderChain();
    showBreath(null);
    setBreathInfoVisible(false);
    closeRetrievedPanel();
};

// onUserTyping(): called from gameList's input handler the instant the
// user starts editing the bar again. Dismisses the big top-third breath
// so it doesn't linger beside fresh fuzzy results, and graduates the
// current live AI response into the decaying chain below the bar so
// the line isn't lost — it just moves to where recent memories live.
export const onUserTyping = () => {
    const el = breathEl();
    if (!el || !el.classList.contains('is-visible')) return;
    clearBreathHold();
    if (liveAi) {
        pushTurn('ai', liveAi, liveRetrieved);
        liveAi = null;
        liveRetrieved = null;
    }
    showBreath(null);
    setBreathInfoVisible(false);
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
    const info = breathInfoEl();
    if (info) {
        info.addEventListener('click', () => {
            if (liveRetrieved && liveRetrieved.length) openRetrievedPanel(liveRetrieved);
        });
    }
    const close = panelCloseEl();
    if (close) close.addEventListener('click', closeRetrievedPanel);
    const backdrop = panelBackdropEl();
    if (backdrop) backdrop.addEventListener('click', closeRetrievedPanel);
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && !panelEl()?.classList.contains('hidden')) {
            closeRetrievedPanel();
        }
    });
};
