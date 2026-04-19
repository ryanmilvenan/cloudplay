// Hydrate-progress overlay — rendered while pkg/worker/romcache is
// extracting + repacking a .7z-archived ROM before the emulator boots.
//
// The backend fires RoomHydrateProgress messages through the same
// data-channel path as RoomMembers / AchievementUnlocked; wiring.js
// dispatches to show() for each update and hide() when stage === 'done'.
//
// Shape on the wire (api.RoomHydrateProgressResponse):
//   stage   — 'start' | 'extract' | 'repack' | 'done'
//   percent — 0..100 when quantifiable, -1 when not (e.g. repack)
//   extras  — free-form hint, shown under the progress bar

const el = () => document.getElementById('hydrate-overlay');
const barEl = () => document.getElementById('hydrate-overlay-bar');
const titleEl = () => document.getElementById('hydrate-overlay-title');
const subEl = () => document.getElementById('hydrate-overlay-sub');

const titleFor = (stage) => {
    switch (stage) {
        case 'start':   return 'Preparing game…';
        case 'extract': return 'Decompressing archive';
        case 'repack':  return 'Building disc image';
        case 'done':    return 'Ready';
        default:        return 'Loading…';
    }
};

export const show = ({stage, percent, extras}) => {
    const root = el();
    if (!root) return;
    if (stage === 'done') {
        hide();
        return;
    }
    root.classList.remove('hidden');
    titleEl().textContent = titleFor(stage);
    subEl().textContent = extras || '';
    const bar = barEl();
    if (percent >= 0) {
        bar.classList.remove('hydrate-overlay__bar--indeterminate');
        bar.style.width = percent + '%';
    } else {
        // Indeterminate: animate a pulsing stripe so the user sees
        // activity even though we don't know the %.
        bar.classList.add('hydrate-overlay__bar--indeterminate');
        bar.style.width = '100%';
    }
};

export const hide = () => {
    const root = el();
    if (!root) return;
    root.classList.add('hidden');
    barEl().style.width = '0%';
    barEl().classList.remove('hydrate-overlay__bar--indeterminate');
};
