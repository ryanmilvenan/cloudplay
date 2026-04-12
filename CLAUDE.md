# Working in this repo

## Keep the architecture diagrams current

Structural changes MUST update the matching diagram in the same commit:

- **Frontend** (changes under `web/`) → `docs/arch-frontend.md`
- **Backend** (changes under `pkg/`, `cmd/`, `Dockerfile*`, `scripts/`, `config/`) → `docs/arch-backend.md`

### What counts as structural

Update the diagram when you:

- Add, remove, or rename a module, package, or major directory.
- Change the direction of a dependency (e.g., move where a pub/sub flows, relocate a writer, invert an import).
- Add or remove a cross-cutting protocol (new event name, new RPC endpoint, new bus, new sub()).
- Change a deployment path (new bind mount, new build stage, new service split, moving an asset into or out of the image).
- Split or merge a file that crossed the 200-line target.
- Add or remove an invariant listed in the diagram's "Notable invariants" section.

### What does NOT require a diagram update

- Bug fixes that don't change structure.
- Refactors within a single module.
- Comment, docstring, or variable-name edits.
- Config or constant tweaks.
- Adding tests.

### Judgment call

If someone reading the `arch-*.md` file would form a materially different mental model after your change, update it. When in doubt, update it — the diagram's value is as a lagging indicator of architectural drift, and drifting diagrams lose trust fast.

### Commit message convention

When you update a diagram alongside code, the commit message body should mention the diagram explicitly, e.g.:

> refactor(ui): split overlay into panel + slot-picker modules
>
> Diagram updated (docs/arch-frontend.md): overlay.js split into
> overlay/panel.js + overlay/slotPicker.js.

This makes the git log a searchable record of how the architecture evolved.

## Other conventions

- Don't add files under `assets/games/` to git — they're gitignored (ROMs live on the deployment target, not in the repo).
- Every `?v=` import string in the frontend must be `?v=__V__`; `scripts/version.sh` stamps them at deploy time.
- Prefer `/deploy-cloudplay-frontend` for web-only changes (seconds, no rebuild) and `/deploy-cloudplay` only when Go / C / Dockerfile / systemd changes.
- Do not commit generated version stamps — they only live on moon's working tree after a deploy.
