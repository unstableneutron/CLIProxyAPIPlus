# Upstream-Sync Skill Hardening - 2026-06-20

## RED Pressure Scenarios

Baseline skill: `.agents/skills/validating-upstream-sync/SKILL.md` at word count 481.

Pressure scenarios:

1. A resolved sync PR merges, then normal-mode acceptance selects newer `original`
   or Plus refs than the PR preview contained.
2. A Git auto-merge has no textual conflicts but changes fork-owned or Plus-owned
   behavior.
3. A blocked overlay report is produced but the workflow still exits
   successfully.
4. A release appears green, but tag/main/GoReleaser/assets/Docker/local fetched
   state were not all verified.
5. A future edit to `.github/scripts/upstream-sync.sh` bypasses the checked-in
   ownership and invariant manifests.

Observed current-skill gaps before editing:

- It warns that green workflow success is not enough.
- It requires normal-mode `force_pr=false` acceptance after merging.
- It lists tag, GoReleaser, release asset, Docker, local `main`, and tag
  evidence.
- It does not mention `.github/scripts/test-upstream-sync.sh`.
- It does not mention `.github/scripts/upstream-sync.sh check-invariants`.
- It does not mention `shellcheck` or `actionlint`.
- It does not name `.github/upstream-sync-ownership.tsv` or
  `.github/upstream-sync-invariants.tsv` as checked-in gates.
- It does not name non-conflicting owned-path clobber detection.
- It does not say blocked overlay reporting must fail after reporting.
- It does not explicitly require diagnostic build/test tails when a gate fails.

Initial decision: update `validating-upstream-sync`. Do not create broader skills
unless validation shows a reusable gap outside this repo's upstream-sync workflow.

## GREEN Validation

Edited `.agents/skills/validating-upstream-sync/SKILL.md` to cover:

- Normal-mode acceptance must compare selected refs with PR-reviewed refs.
- Local gates now list helper tests, invariant checks, `shellcheck`,
  `actionlint`, compile, and full Go tests.
- Ownership and invariant manifests are explicit validation inputs.
- Non-conflicting owned-path clobbers are called out as a failure mode.
- Blocked non-force sync reports must fail after reporting.
- Release proof must tie upstream-sync, GoReleaser, Docker, release assets,
  local tag, and `main` to the same tag/main state.

Validation evidence:

- RED subagent pressure review found current-skill gaps for acceptance ref
  drift, owned clobber reporting, blocked-report fail-after-report semantics,
  and same-tag release-chain proof.
- `wc -w .agents/skills/validating-upstream-sync/SKILL.md` -> 499 words.
- Frontmatter syntax check passed: `name` uses skill-name characters and
  `description` starts with `Use when`.
- Trigger grep found required terms for helper tests, invariants, shellcheck,
  actionlint, ownership, clobbers, `force_pr=false`, GoReleaser, Docker, and
  selected refs.
- `.github/scripts/test-upstream-sync.sh` passed.
- `.github/scripts/upstream-sync.sh check-invariants` passed.
- `shellcheck .github/scripts/upstream-sync.sh .github/scripts/test-upstream-sync.sh` passed.
- `go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/upstream-sync.yml` passed.
- `go build -o test-output ./cmd/server && rm test-output` passed.
- `go test ./...` passed.
- GREEN changed-files pressure review found no touched-scope issues and agreed
  that the edited skill covers the requested failure modes.

New skill decision:

- No new skills created. The RED/GREEN evidence points to repo-specific
  upstream-sync acceptance semantics, ownership manifests, invariant manifests,
  and release dispatch behavior. Those belong in `validating-upstream-sync` and
  `AGENTS.md`, not in broad reusable skills yet.
