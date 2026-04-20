/**
 * Game-select screen (search-first, AI-augmented).
 *
 * Clean rewrite of the cloudretro-inherited game list. The previous
 * implementation fought with mobile focus because the layout was driven
 * by JS-resized viewport math, absolute-positioned siblings overlapping
 * the search row, and synthetic input events dispatched from click
 * handlers. This module strips all of that out.
 *
 * Design:
 *   - Layout is 100% CSS grid keyed on 100dvh. When the on-screen
 *     keyboard appears and the dynamic viewport shrinks, the grid
 *     reflows naturally — nothing in here cares. No `resize` listener,
 *     no viewport-locking of #gamebody, no padding-top: vh hack.
 *   - Input is wrapped in a native `<label>`. Tapping anywhere in the
 *     search row focuses the input through native browser behaviour —
 *     no JS tap-to-focus stack, no pointerdown handlers, no synthetic
 *     events. This is what iOS Safari needs to keep the keyboard up.
 *   - State is one object, render is one function reading that state.
 *     Events mutate state and call render(). No independent DOM paths.
 *   - Search is fuzzy (instant) blended with semantic (debounced) via
 *     reciprocal-rank fusion — unchanged from the prior version, just
 *     lifted out of the DOM/lifecycle code.
 *   - Old file preserved at web/js/gameList.legacy.js for reference
 *     during the stabilization window; not imported anywhere.
 *
 * Export surface preserved for wiring.js / session.js / lifecycle.js:
 *   set, show, hide, render, scroll, select, disable,
 *   onStart (setter), selected (getter), selectedGame (getter),
 *   isEmpty (getter), findByTitle
 */

import { filter as fuzzyFilter } from './fuzzy.js?v=__V__';
import {
    ask as aiAsk,
    getMode as aiGetMode,
    init as aiInit,
    clearConversation as aiClearConversation,
    onUserTyping as aiOnUserTyping,
    setLaunchHandler as aiSetLaunchHandler,
} from './aiSearch.js?v=__V__';
import { semanticSearch } from './network/semanticSearch.js?v=__V__';

// ── DOM ────────────────────────────────────────────────────────────────
const screenEl = document.getElementById('game-list-screen');
const inputEl = document.getElementById('game-select-input');
const resultsEl = document.getElementById('game-select-results');

// ── Config ─────────────────────────────────────────────────────────────
const SYSTEM_LABELS = {
    gc: 'GameCube', wii: 'Wii', dreamcast: 'Dreamcast',
    snes: 'SNES', nes: 'NES', gba: 'Game Boy Advance',
    pcsx: 'PlayStation', ps2: 'PlayStation 2', n64: 'Nintendo 64',
    mame: 'Arcade', dos: 'DOS', xbox: 'Xbox',
};
const systemLabel = (s = '') => SYSTEM_LABELS[s] || (s ? s.toUpperCase() : 'Other');

const SEMANTIC_DEBOUNCE_MS = 250;
const SEMANTIC_TOP = 20;
const RRF_K = 60;
const SCROLL_INTERVAL_MS = 180;
const MAX_RESULTS = 50;

// ── State ──────────────────────────────────────────────────────────────
const state = {
    library: [],
    filtered: [],
    selected: 0,
    disabled: false,
    onStart: () => {},
};

// ── Rendering ──────────────────────────────────────────────────────────
const escapeHtml = (s = '') => String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
})[c]);

function renderResults() {
    if (!resultsEl) return;
    const { filtered, selected } = state;
    resultsEl.classList.toggle('has-results', filtered.length > 0);
    resultsEl.innerHTML = filtered.map((game, i) => {
        const isSel = i === selected;
        const cover = game.cover_url
            ? `<img class="game-select__result-cover" src="${escapeHtml(game.cover_url)}" alt="" loading="lazy" onerror="this.remove()">`
            : `<div class="game-select__result-cover game-select__result-cover--empty"></div>`;
        return `<div class="game-select__result${isSel ? ' is-selected' : ''}" data-index="${i}" role="option" aria-selected="${isSel}">`
             + cover
             + `<div class="game-select__result-body">`
             +   `<div class="game-select__result-title">${escapeHtml(game.alias || game.title)}</div>`
             +   `<div class="game-select__result-system">${escapeHtml(systemLabel(game.system))}</div>`
             + `</div></div>`;
    }).join('');
    // Keep the selected row visible when the keyboard/gamepad walks off-screen.
    resultsEl.querySelector('.game-select__result.is-selected')
        ?.scrollIntoView({ block: 'nearest' });
}

// ── Search (fuzzy + semantic RRF blend) ────────────────────────────────
let semanticTimer = null;
let semanticGen = 0;

function applyQuery(raw) {
    const query = (raw || '').trim();
    clearTimeout(semanticTimer);

    // Empty bar, or AI mode: dropdown stays closed. AI mode turns the
    // bar into a prompt, not a filter — the chain/breath own the visual
    // feedback while typing.
    if (!query || aiGetMode()) {
        state.filtered = [];
        state.selected = 0;
        renderResults();
        return;
    }

    // Fuzzy first — purely local, instant.
    const fuzzy = fuzzyFilter(
        state.library, query,
        (g) => `${g.alias || ''} ${g.title}`,
    ).slice(0, MAX_RESULTS);
    state.filtered = fuzzy;
    state.selected = 0;
    renderResults();

    // Semantic in the background, blended in on arrival.
    const gen = ++semanticGen;
    semanticTimer = setTimeout(async () => {
        const hits = await semanticSearch(query, SEMANTIC_TOP);
        if (gen !== semanticGen || !hits.length) return;
        state.filtered = blend(fuzzy, hits);
        renderResults();
    }, SEMANTIC_DEBOUNCE_MS);
}

// Reciprocal-rank fusion: each list contributes 1/(RRF_K + rank) per
// game, scores sum across lists, top-N wins. k=60 is the TREC default.
function blend(fuzzy, semantic) {
    const byKey = new Map();
    const add = (game, rrf) => {
        const k = `${game.path}|${game.system}`;
        const cur = byKey.get(k);
        if (cur) cur.score += rrf;
        else byKey.set(k, { game, score: rrf });
    };
    fuzzy.forEach((g, i) => add(g, 1 / (RRF_K + i)));
    const libByKey = new Map(state.library.map(g => [`${g.path}|${g.system}`, g]));
    semantic.forEach((h, i) => {
        const g = libByKey.get(`${h.game_path}|${h.system}`);
        if (g) add(g, 1 / (RRF_K + i));
    });
    return [...byKey.values()]
        .sort((a, b) => b.score - a.score)
        .slice(0, MAX_RESULTS)
        .map(e => e.game);
}

// ── Navigation + launch ────────────────────────────────────────────────
function stepSelection(dir) {
    if (state.filtered.length === 0) return;
    const n = state.filtered.length;
    state.selected = ((state.selected + dir) % n + n) % n;
    renderResults();
}

function launchSelected() {
    if (state.filtered.length === 0) return;
    aiClearConversation();
    state.onStart();
}

function handleEnter() {
    const q = inputEl.value;
    if (aiGetMode()) {
        // In AI mode Enter hands the query to the conversational agent.
        // We clear the bar so the breath has the stage; the chain will
        // echo the query below.
        inputEl.value = '';
        applyQuery('');
        aiAsk(q);
        return;
    }
    launchSelected();
}

// ── Mount ──────────────────────────────────────────────────────────────
let mounted = false;
function mount() {
    if (mounted || !inputEl) return;
    mounted = true;

    aiInit();
    aiSetLaunchHandler((action) => {
        const key = `${action.game_path}|${action.system}`;
        const idx = state.library.findIndex(g => `${g.path}|${g.system}` === key);
        if (idx < 0) return;
        state.filtered = [state.library[idx]];
        state.selected = 0;
        renderResults();
        state.onStart();
    });

    inputEl.addEventListener('input', (e) => {
        if (state.disabled) return;
        // First keystroke dismisses the big breath. Previous AI response
        // (if any) graduates into the decaying chain so it's still
        // reachable via the info tooltip.
        aiOnUserTyping();
        applyQuery(e.target.value);
    });

    inputEl.addEventListener('keydown', (e) => {
        if (state.disabled) return;
        if (e.key === 'ArrowDown') { e.preventDefault(); stepSelection(1); return; }
        if (e.key === 'ArrowUp')   { e.preventDefault(); stepSelection(-1); return; }
        if (e.key === 'Enter')     { e.preventDefault(); handleEnter(); }
    });

    resultsEl?.addEventListener('click', (e) => {
        if (state.disabled) return;
        const row = e.target.closest('.game-select__result');
        if (!row) return;
        const i = Number(row.dataset.index);
        if (!Number.isFinite(i)) return;
        state.selected = i;
        renderResults();
        launchSelected();
    });
}

// ── Gamepad/arrow-key held-scroll ──────────────────────────────────────
let scrollTimer = null;

// ── External API (shape preserved for existing callers) ────────────────
export const gameList = {
    set(games = []) {
        state.library = [...games].sort((a, b) =>
            (a.title || '').toLowerCase() > (b.title || '').toLowerCase() ? 1 : -1
        );
        screenEl?.classList.remove('loading');
        mount();
        // Re-filter in case the user has been typing while the library
        // streamed in (e.g. IGDB Phase 2 delta enrichment).
        if (inputEl) applyQuery(inputEl.value);
    },
    get selected() {
        return state.filtered[state.selected]?.title || '';
    },
    get selectedGame() {
        return state.filtered[state.selected] || null;
    },
    show() {
        if (screenEl) screenEl.style.display = '';
        // Reset everything the previous session may have left behind:
        // startGame() calls disable() and leaves the filtered list
        // pointing at the launched game, which — once we're back at
        // the menu after leaving — would otherwise show a stale Halo
        // row stuck in the dropdown AND swallow every keystroke because
        // state.disabled is still true. Clearing here gives a clean
        // slate without callers having to know about these internals.
        state.disabled = false;
        state.filtered = [];
        state.selected = 0;
        if (inputEl) inputEl.value = '';
        clearTimeout(semanticTimer);
        renderResults();
        mount();
    },
    hide() {
        if (screenEl) screenEl.style.display = 'none';
    },
    render() { renderResults(); },
    scroll(direction) {
        clearInterval(scrollTimer);
        if (direction === 0) return;
        stepSelection(direction);
        scrollTimer = setInterval(() => stepSelection(direction), SCROLL_INTERVAL_MS);
    },
    select(index) {
        if (state.filtered.length === 0) return;
        const n = state.filtered.length;
        state.selected = ((index % n) + n) % n;
        renderResults();
    },
    disable() {
        state.disabled = true;
        clearInterval(scrollTimer);
    },
    set onStart(fn) { state.onStart = fn; },
    get isEmpty() { return state.library.length === 0; },
    findByTitle(title) {
        if (!title) return null;
        const lower = title.toLowerCase();
        return state.library.find(g => (g.title || '').toLowerCase() === lower) || null;
    },
};
