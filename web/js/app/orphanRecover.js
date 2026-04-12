// Orphaned-session recovery button.
//
// Hidden for the first ORPHAN_DELAY_MS so normal fast connections don't
// flash it. After the delay, if still in "Connecting..." (.loading),
// it becomes visible. Disappears permanently once the game list
// populates (loading class is removed). Clicking reloads without a
// room id so the coordinator assigns a fresh worker.

export const initOrphanRecover = () => {
    const ORPHAN_DELAY_MS = 6000;
    const btn = document.getElementById('orphan-recover-btn');
    if (!btn) return;

    btn.style.display = 'none';

    const screenEl = document.getElementById('game-list-screen');

    const showTimer = setTimeout(() => {
        if (screenEl && screenEl.classList.contains('loading')) {
            btn.style.removeProperty('display');
        }
    }, ORPHAN_DELAY_MS);

    const observer = new MutationObserver(() => {
        if (!screenEl.classList.contains('loading')) {
            clearTimeout(showTimer);
            btn.style.display = 'none';
            observer.disconnect();
        }
    });
    if (screenEl) {
        observer.observe(screenEl, {attributes: true, attributeFilter: ['class']});
    }

    btn.addEventListener('click', () => {
        try { localStorage.removeItem('roomID'); } catch (_) {}
        const cleanUrl = window.location.origin + window.location.pathname;
        window.location.replace(cleanUrl);
    });
};
