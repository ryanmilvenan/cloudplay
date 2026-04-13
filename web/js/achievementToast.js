// Rich achievement-unlock toast. Fixed top-right, stacks newest-on-top,
// auto-dismisses after HOLD_MS. Rendered from a minimal data object —
// callers don't touch the DOM directly.
//
// Also exposes a `demo()` helper you can call from devtools to see the
// layout without earning a real unlock.

const HOLD_MS = 5200;   // time fully visible
const FADE_MS = 240;    // transition duration (matches CSS)
const MAX_STACK = 4;    // drop oldest once this many are on screen

const container = document.getElementById('achievement-toasts');

/**
 * @typedef {object} Unlock
 * @property {number} [id]
 * @property {string} title
 * @property {string} [description]
 * @property {number} [points]
 * @property {string} [badge_url]
 */

const render = (u) => {
    const el = document.createElement('div');
    el.className = 'achievement-toast';
    el.setAttribute('role', 'status');

    const badge = document.createElement('div');
    badge.className = 'achievement-toast__badge';
    if (u.badge_url) {
        badge.style.backgroundImage = `url(${u.badge_url})`;
    }

    const body = document.createElement('div');
    body.className = 'achievement-toast__body';

    const heading = document.createElement('div');
    heading.className = 'achievement-toast__heading';
    heading.textContent = 'Achievement Unlocked';

    const title = document.createElement('div');
    title.className = 'achievement-toast__title';
    title.textContent = u.title || 'Achievement';
    if (typeof u.points === 'number' && u.points > 0) {
        const pts = document.createElement('span');
        pts.className = 'achievement-toast__title-points';
        pts.textContent = `+${u.points}`;
        title.appendChild(pts);
    }

    body.append(heading, title);
    if (u.description) {
        const desc = document.createElement('div');
        desc.className = 'achievement-toast__desc';
        desc.textContent = u.description;
        body.appendChild(desc);
    }

    el.append(badge, body);
    return el;
};

const dismiss = (el) => {
    el.classList.remove('is-visible');
    setTimeout(() => el.remove(), FADE_MS + 40);
};

export const show = (u) => {
    if (!container || !u) return;
    const el = render(u);
    container.insertBefore(el, container.firstChild);

    // Trim stack from the bottom (oldest) if we exceed MAX_STACK.
    while (container.children.length > MAX_STACK) {
        dismiss(container.lastElementChild);
    }

    // Paint with opacity 0 / translate, then transition in.
    requestAnimationFrame(() => el.classList.add('is-visible'));
    setTimeout(() => dismiss(el), HOLD_MS);
};

/** Devtools helper: show a stubbed unlock to preview the layout. */
export const demo = () => show({
    title: 'Welcome to the Arena',
    description: 'Complete the tutorial and take your first step.',
    points: 10,
    badge_url: 'https://media.retroachievements.org/Badge/227773.png',
});

// Expose a global for quick devtools probing: window.__achievementToast.demo()
if (typeof window !== 'undefined') {
    window.__achievementToast = {show, demo};
}
