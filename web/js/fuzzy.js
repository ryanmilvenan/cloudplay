// Tiny zero-dep fuzzy scorer used by gameList's search-first UI.
//
// Contract: score(title, query) returns a non-negative score. Higher =
// better. 0 means no match; the caller filters those out. Designed for
// thousands of candidates on every keystroke without stutter.
//
// Scoring rules:
//   - Case-insensitive.
//   - Normalize both sides: strip " [!]", " (USA)" / " (Rev N)" / " (Disc N)"
//     wrappers, collapse whitespace, lower-case, normalize roman numerals
//     I → 1, II → 2, III → 3, IV → 4 (covers Halo II → "halo 2", etc.).
//   - All whitespace-separated query tokens must appear in the title. Any
//     missing token → score 0.
//   - Bonus: +100 if the first token matches at word-start.
//   - Bonus: +50 if the whole query is a contiguous substring of the title.
//   - Penalty: -position of the first token's match inside the title.
//
// The returned score is therefore dominated by how "head-matched" the
// query is; two hits with identical substring overlap rank by where in
// the title the match starts.

const WRAP_TAGS_RE = /\s*(?:\[[^\]]*\]|\([^)]*\))/g;
const WHITESPACE_RE = /\s+/g;
const ROMANS = { i: '1', ii: '2', iii: '3', iv: '4', v: '5', vi: '6', vii: '7', viii: '8', ix: '9', x: '10' };

export const normalize = (s = '') => {
    return s
        .replace(WRAP_TAGS_RE, ' ')
        .toLowerCase()
        .replace(WHITESPACE_RE, ' ')
        .trim()
        .split(' ')
        .map(tok => ROMANS[tok] || tok)
        .join(' ');
};

export const score = (title, query) => {
    const nTitle = normalize(title);
    const nQuery = normalize(query);
    if (!nQuery) return 0;
    const tokens = nQuery.split(' ').filter(Boolean);
    if (tokens.length === 0) return 0;

    // Every token must appear somewhere in the normalized title.
    for (const t of tokens) {
        if (!nTitle.includes(t)) return 0;
    }

    let s = 100; // base

    // Head-match bonus: first token at start of title or after a space.
    const firstTokenIdx = nTitle.indexOf(tokens[0]);
    if (firstTokenIdx === 0 || nTitle[firstTokenIdx - 1] === ' ') {
        s += 100;
    }

    // Contiguous-query bonus.
    if (nTitle.includes(nQuery)) {
        s += 50;
    }

    // Position penalty — earlier matches rank higher.
    s -= firstTokenIdx;

    return Math.max(s, 1);
};

// filter: return entries of `items` whose score(keyFn(item), q) > 0,
// sorted best-first. Items that tie in score keep input order (stable).
export const filter = (items, q, keyFn = (it) => it.title) => {
    const scored = [];
    for (let i = 0; i < items.length; i++) {
        const sc = score(keyFn(items[i]), q);
        if (sc > 0) scored.push({ i, sc });
    }
    scored.sort((a, b) => b.sc - a.sc || a.i - b.i);
    return scored.map(({ i }) => items[i]);
};

// isNaturalLanguageQuery: used by aiSearch to decide whether Enter in
// AI mode should engage the LLM (Phase 4) or (in fuzzy mode) just
// launch the top hit. Phase 1 always routes AI-mode Enter through the
// breath, but Phase 4 may still use this to skip the LLM for obviously
// direct queries. Exported so the test suite can nail down the
// heuristic before Phase 4 depends on it.
const FILLERS = new Set([
    'i', 'want', 'to', 'play', 'lets', "let's", 'can', 'we', 'show',
    'me', 'find', 'like', 'similar', 'something', 'what', 'which',
    'how', 'about', 'maybe', 'any',
]);
export const isNaturalLanguageQuery = (q = '') => {
    const trimmed = q.trim();
    if (!trimmed) return false;
    if (trimmed.includes('?')) return true;
    const tokens = trimmed.toLowerCase().split(/\s+/);
    if (tokens.length >= 5) return true;
    for (const t of tokens) {
        if (FILLERS.has(t)) return true;
    }
    return false;
};
