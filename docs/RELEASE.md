# Tag-driven releases

A Git tag is the release version source. There is no source-version edit or
manual “Create release” step. You may tag an existing tested commit without
making an empty commit. The commit must contain this release workflow.

1. Push the intended commit and wait for its normal CI checks.
2. Choose a new unused `vX.Y.Z` or `vX.Y.Z-rc.1` tag and point it at that exact commit:

   ```sh
   git tag -a vX.Y.Z <tested-commit> -m 'Release vX.Y.Z'
   git push origin vX.Y.Z
   ```

3. The tag push reruns the complete native amd64 and arm64 quality gates.
   After both pass, packaging validates both artifacts, creates/reuses a draft
   Release, uploads and verifies the complete asset set, then publishes it.
   A suffix automatically marks the Release as a prerelease.

Branch pushes, pull requests and manual branch runs only validate; they do not
publish. There is no second `release.published` trigger. The tag build injects
its numeric version into Go with linker flags. For CPA compatibility the inner
library remains `cmdcode-go-vX.Y.Z.so`, including for prereleases. The full tag
is retained in `build_tag` metadata, archive names and release manifests.
Non-tag builds use version `0.0.0` and `build_tag=dev`; these are not releases.
A tag CI run rebuilds and verifies the same source commit with release metadata,
so its bytes differ from the earlier development artifact.

## Assets and verification

For tag `vX.Y.Z` the Release contains both
`cmdcode-go-vX.Y.Z-linux-amd64.tar.gz` and
`cmdcode-go-vX.Y.Z-linux-arm64.tar.gz`, plus
`cmdcode-go-vX.Y.Z-SHA256SUMS` and `cmdcode-go-vX.Y.Z-release.json`.
Verify the outer checksum and the archive's inner `SHA256SUMS` before installing
the architecture-appropriate library. Do not add an architecture or prerelease
suffix to the inner library filename. Each archive retains LICENSE, plugin and
build metadata, ELF/glibc reports, embedded clean commit and per-file hashes.

## Failure and retry

A failed test/build/packaging check cannot publish a Release. Upload failures
leave a draft; retry failed jobs to resume. Identical assets are checked and
skipped. Conflicting existing assets fail without overwriting them. A published
Release is only accepted if its complete expected assets already match; it is
never edited to repair missing/conflicting assets. Empty failed-upload starter
assets may be cleaned up only in a draft. Tag identity is rechecked throughout;
no code creates or moves tags. Successful retries do not publish a second copy.
Build-run provenance can differ on a full rerun; prefer retrying failed jobs.
Immutable releases are supported by attaching assets before publication.

Tests use fake GitHub clients and synthetic ELF packaging fixtures, plus actual
Go linker-injection checks. The native gates additionally test Go/race/vet,
model extraction, workflows, C ABI lifecycle and provenance. No live provider
credentials or inference calls are required. See [CI details](CI.md).

## Model data and releases

`models-data` refreshes independently without tags or plugin releases. Each code
release pins `data/models.snapshot.json` and its reproducible generated Go fallback;
builds never download a moving data branch. The first migration must seed the data
branch before deploying clients with the new default URL. See [MODELS.md](MODELS.md).
