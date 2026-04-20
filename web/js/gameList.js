/**
 * Search-first game select UI — the flat grid that used to render every
 * game is gone. In its place: a single search input that fuzzy-filters
 * the library live as you type, a grayed AI turn chain and a breath
 * line above/below that bar, and a dropdown of matching results.
 *
 * External surface preserved for wiring.js, session.js, lifecycle.js:
 *   set(games)          — populate library from LibNewGameList broadcast
 *   show() / hide()     — screen visibility
 *   render()            — re-render current filter
 *   scroll(dir)         — gamepad axis / arrow-key navigation through results
 *   select(index)       — external selection (used rarely; still kept)
 *   disable()           — stop responding (called on start)
 *   onStart = fn        — launch callback
 *   selected            — title of selected game (CDP driver reads this)
 *   selectedGame        — full game metadata object (session.js reads this)
 *   isEmpty             — pre-populated? (loading-spinner state in lifecycle)
 *   findByTitle(title)  — room-id → game object lookup
 *
 * Phase 1 scope: fuzzy only. AI toggle and breath UI live in
 * aiSearch.js; that module owns Enter-in-AI-mode routing. Phase 4
 * replaces aiSearch's stub ask() with the live LLM endpoint.
 */

import { filter as fuzzyFilter, isNaturalLanguageQuery } from './fuzzy.js?v=__V__';
import { ask as aiAsk, getMode as aiGetMode, init as aiInit, clearConversation, onUserTyping as aiOnUserTyping } from './aiSearch.js?v=__V__';
import { semanticSearch } from './network/semanticSearch.js?v=__V__';

const screenEl = document.getElementById('game-list-screen');
const inputEl = document.getElementById('game-select-input');
const resultsEl = document.getElementById('game-select-results');

let library = [];         // full list, alphabetical
let filtered = [];        // current filter result (library if query empty)
let selectedIndex = 0;    // index into `filtered`
let onStart = () => {};
let disabled = false;
let enabled = false;      // input + key wiring bound?

const SYSTEM_LABELS = {
    gc: 'GameCube',
    wii: 'Wii',
    dreamcast: 'Dreamcast',
    snes: 'SNES',
    nes: 'NES',
    gba: 'Game Boy Advance',
    pcsx: 'PlayStation',
    ps2: 'PlayStation 2',
    n64: 'Nintendo 64',
    mame: 'Arcade',
    dos: 'DOS',
    xbox: 'Xbox',
};
const systemLabel = (s = '') => SYSTEM_LABELS[s] || (s ? s.toUpperCase() : 'Other');

const DEFAULT_TOP_WHEN_EMPTY = 0; // empty query shows nothing; placeholder guides user

const renderResults = () => {
    if (!resultsEl) return;
    resultsEl.innerHTML = '';
    if (filtered.length === 0) {
        // No results yet — the input's placeholder carries the UX.
        resultsEl.classList.remove('has-results');
        return;
    }
    resultsEl.classList.add('has-results');
    filtered.forEach((game, i) => {
        const el = document.createElement('div');
        el.className = 'game-select__result' + (i === selectedIndex ? ' is-selected' : '');
        el.dataset.index = String(i);
        el.setAttribute('role', 'option');
        el.setAttribute('aria-selected', String(i === selectedIndex));
        el.innerHTML =
            `<div class="game-select__result-title">${escapeHtml(game.alias || game.title)}</div>` +
            `<div class="game-select__result-system">${escapeHtml(systemLabel(game.system))}</div>`;
        el.addEventListener('click', () => {
            selectedIndex = i;
            renderResults();
            onStart();
        });
        resultsEl.appendChild(el);
    });
    scrollSelectedIntoView();
};

const scrollSelectedIntoView = () => {
    const el = resultsEl?.querySelector('.game-select__result.is-selected');
    if (el && typeof el.scrollIntoView === 'function') {
        el.scrollIntoView({ block: 'nearest' });
    }
};

// applyQuery runs both the local fuzzy filter (instant, synchronous)
// and a debounced semantic-search fetch (async, 250 ms after last
// keystroke). The fuzzy hits render immediately so the UI always
// feels responsive; semantic hits merge in as they arrive via
// reciprocal-rank fusion, boosting titles that both paths rank well
// and surfacing titles that only semantic found (e.g. the user types
// "soccer" and we pull in FIFA / Winning Eleven even though the
// token "soccer" appears in none of their file names).
let semanticTimer = null;
let semanticGeneration = 0;
const SEMANTIC_DEBOUNCE_MS = 250;
const SEMANTIC_TOP = 20;

const applyQuery = (q) => {
    const query = (q || '').trim();
    if (!query) {
        filtered = library.slice(0, DEFAULT_TOP_WHEN_EMPTY);
        selectedIndex = 0;
        renderResults();
        return;
    }
    // Fuzzy hits — instant.
    const fuzzy = fuzzyFilter(library, query, (g) => `${g.alias || ''} ${g.title}`).slice(0, 50);
    filtered = fuzzy;
    selectedIndex = 0;
    renderResults();

    // Semantic hits — debounced. Stale requests are dropped by
    // comparing generation counters; a slow response to "fifa" won't
    // clobber a fresh response to "fifa 0".
    clearTimeout(semanticTimer);
    const gen = ++semanticGeneration;
    semanticTimer = setTimeout(async () => {
        const hits = await semanticSearch(query, SEMANTIC_TOP);
        if (gen !== semanticGeneration || !hits.length) return;
        filtered = blendFuzzyAndSemantic(fuzzy, hits, query);
        renderResults();
    }, SEMANTIC_DEBOUNCE_MS);
};

// blendFuzzyAndSemantic merges the two result sets via reciprocal-rank
// fusion (RRF): score(game) = sum over lists of 1/(k + rank). k=60 is
// the canonical TREC default; it down-weights tail ranks so a list of
// 50 fuzzy hits doesn't drown out 10 strong semantic hits.
const RRF_K = 60;
const blendFuzzyAndSemantic = (fuzzy, semantic, query) => {
    const byKey = new Map(); // path|system -> {game, score}
    const addOrUpdate = (game, rrf) => {
        const key = `${game.path}|${game.system}`;
        const cur = byKey.get(key);
        if (cur) cur.score += rrf;
        else byKey.set(key, { game, score: rrf });
    };
    fuzzy.forEach((g, i) => addOrUpdate(g, 1 / (RRF_K + i)));
    // semantic hits are {game_path, system, score} — look up the full
    // game object from the library by (path, system).
    const libByKey = new Map(library.map(g => [`${g.path}|${g.system}`, g]));
    semantic.forEach((h, i) => {
        const game = libByKey.get(`${h.game_path}|${h.system}`);
        if (!game) return;
        addOrUpdate(game, 1 / (RRF_K + i));
    });
    return [...byKey.values()]
        .sort((a, b) => b.score - a.score)
        .slice(0, 50)
        .map(e => e.game);
};

const bindOnce = () => {
    if (enabled) return;
    enabled = true;
    aiInit();
    if (!inputEl) return;
    inputEl.addEventListener('input', (e) => {
        if (disabled) return;
        // As soon as the user edits the bar, dismiss the big top-third
        // breath — the AI response (if any) graduates into the decaying
        // chain where recent memories live, so nothing is lost visually.
        aiOnUserTyping();
        applyQuery(e.target.value);
    });
    inputEl.addEventListener('keydown', (e) => {
        if (disabled) return;
        if (e.key === 'ArrowDown') { e.preventDefault(); stepSelection(1); return; }
        if (e.key === 'ArrowUp')   { e.preventDefault(); stepSelection(-1); return; }
        if (e.key === 'Enter')     { e.preventDefault(); handleEnter(); }
    });
};

const stepSelection = (dir) => {
    if (filtered.length === 0) return;
    const n = filtered.length;
    selectedIndex = ((selectedIndex + dir) % n + n) % n;
    renderResults();
};

const handleEnter = () => {
    const q = inputEl.value;
    const aiOn = aiGetMode();
    // AI mode takes priority when ON: any Enter routes through the
    // conversational agent (Phase 1 renders a stub). Natural-language
    // heuristic may be consulted in Phase 4 to short-circuit obvious
    // direct queries, but right now we hand everything to the agent.
    if (aiOn) {
        // Fuzzy still applies live while typing; pressing Enter is the
        // user saying "interpret this as a sentence, not a filter".
        inputEl.value = '';
        applyQuery('');
        aiAsk(q);
        // Intentionally referenced so bundlers / IDE-grepping know the
        // heuristic is used by the wiring even when we don't branch on
        // it here. Phase 4 will use its return value.
        void isNaturalLanguageQuery(q);
        return;
    }
    // Fuzzy mode: Enter launches the currently-selected match (top hit
    // if user didn't navigate).
    if (filtered.length === 0) return;
    clearConversation(); // if any prior AI turns sit in the chain, drop them
    onStart();
};

const escapeHtml = (s = '') =>
    s.replace(/[&<>"']/g, (c) => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    })[c]);

// Gamepad/keyboard axis driver — lifecycle.js calls scroll(±1) to step
// and scroll(0) to stop. We fire one step immediately, then an interval
// mirrors the old behavior for held inputs.
let scrollTimer = null;
const SCROLL_INTERVAL_MS = 180;

export const gameList = {
    set(games = []) {
        // Stable alphabetic base.
        library = [...games].sort((a, b) =>
            (a.title || '').toLowerCase() > (b.title || '').toLowerCase() ? 1 : -1
        );
        screenEl?.classList.remove('loading');
        bindOnce();
        // Re-apply whatever the user currently has typed — new library
        // might surface matches that weren't there before (IGDB Phase 2
        // delta, 7z hydration deleting an archive, etc.).
        if (inputEl) applyQuery(inputEl.value);
    },
    get selected() {
        return filtered[selectedIndex]?.title || '';
    },
    get selectedGame() {
        return filtered[selectedIndex] || null;
    },
    show() {
        if (screenEl) screenEl.style.display = '';
        bindOnce();
    },
    hide() {
        if (screenEl) screenEl.style.display = 'none';
    },
    render() {
        renderResults();
    },
    scroll(direction) {
        clearInterval(scrollTimer);
        if (direction === 0) return;
        stepSelection(direction);
        scrollTimer = setInterval(() => stepSelection(direction), SCROLL_INTERVAL_MS);
    },
    select(index) {
        if (filtered.length === 0) return;
        const n = filtered.length;
        selectedIndex = ((index % n) + n) % n;
        renderResults();
    },
    disable() {
        disabled = true;
        clearInterval(scrollTimer);
    },
    set onStart(fn) { onStart = fn; },
    get isEmpty() { return library.length === 0; },
    findByTitle(title) {
        if (!title) return null;
        const lower = title.toLowerCase();
        return library.find(g => (g.title || '').toLowerCase() === lower) || null;
    },
};
