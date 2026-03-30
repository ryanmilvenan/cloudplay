/**
 * New game list UI — full-screen dark scrollable list.
 *
 * Renders into #game-list-container. Supports keyboard (arrow up/down + Enter)
 * and gamepad navigation (via the existing scroll/select interface).
 */

const containerEl = document.getElementById('game-list-container');
const screenEl = document.getElementById('game-list-screen');

let games = [];
let selectedIndex = 0;
let onStart = () => {};

const render = () => {
    containerEl.innerHTML = '';
    games.forEach((game, i) => {
        const el = document.createElement('div');
        el.className = 'game-list-item' + (i === selectedIndex ? ' selected' : '');
        el.innerHTML =
            `<div class="game-list-item__title">${game.alias || game.title}</div>` +
            `<div class="game-list-item__system">${game.system || ''}</div>`;
        el.addEventListener('click', () => {
            select(i);
            onStart();
        });
        containerEl.appendChild(el);
    });
    scrollIntoView();
};

const select = (index) => {
    if (games.length === 0) return;
    selectedIndex = ((index % games.length) + games.length) % games.length;
    const items = containerEl.querySelectorAll('.game-list-item');
    items.forEach((el, i) => el.classList.toggle('selected', i === selectedIndex));
    scrollIntoView();
};

const scrollIntoView = () => {
    const items = containerEl.querySelectorAll('.game-list-item');
    if (items[selectedIndex]) {
        items[selectedIndex].scrollIntoView({block: 'nearest', behavior: 'smooth'});
    }
};

const show = () => {
    screenEl.style.display = '';
    if (games.length > 0) render();
};

const hide = () => {
    screenEl.style.display = 'none';
};

// Scroll interface compatible with existing gamepad axis handling
// direction: -1 (up), 1 (down), 0 (stop)
let scrollInterval = null;
const SCROLL_INTERVAL_MS = 180;

const scroll = (direction) => {
    clearInterval(scrollInterval);
    if (direction === 0) return;
    select(selectedIndex + direction);
    scrollInterval = setInterval(() => select(selectedIndex + direction), SCROLL_INTERVAL_MS);
};

export const gameListNew = {
    set: (data = []) => {
        games = data.sort((a, b) =>
            a.title.toLowerCase() > b.title.toLowerCase() ? 1 : -1
        );
        screenEl.classList.remove('loading');
        if (games.length > 0) render();
    },
    get selected() {
        return games.length > 0 ? games[selectedIndex].title : '';
    },
    get selectedGame() {
        return games.length > 0 ? games[selectedIndex] : null;
    },
    show,
    hide,
    render,
    scroll,
    select,
    disable: () => {
        clearInterval(scrollInterval);
    },
    set onStart(fn) { onStart = fn; },
    get isEmpty() { return games.length === 0; },
};
