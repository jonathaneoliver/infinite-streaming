<!--
Thanks for the PR. A couple of conventions for this repo:

- Use a conventional-commit prefix in the title: feat:, fix:, docs:, chore:, ci:, refactor:.
  Release Drafter autolabels from these and uses them for changelog grouping. Add a
  `breaking` label by hand if your change is a breaking change.

- The repo squash-merges only — your PR title and this body become the single commit
  message on main. Write the title as you'd want it to read in `git log`.
-->

## Summary
<!-- 1–3 bullets on what this PR does and why. -->

## Why
<!-- The user-visible problem or use case this addresses. Skip if obvious from the title. -->

## Test plan
<!-- How you (or a reviewer) can verify the change works. -->

- [ ] Tested locally via `make run` / `make test-deploy-dev` / etc.
- [ ] Added or updated tests where appropriate (`tests/integration/`).
- [ ] Updated docs (README / docs/ARCHITECTURE.md / docs/API.md) if behaviour or env changed.
- [ ] No personal info, secrets, or per-user URLs in the diff.

## Screenshots
<!-- For UI changes — drag-and-drop a before/after. Skip otherwise. -->
