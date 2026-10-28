# AGENTS.md

This file provides guidance to the AI agent when working with code in this repository.

## What this repo is

Local working clone of apkc (upstream `ajinasokan/apkc`, module path keeps that name). Remotes: `origin` = upstream (read-only reference), `fork` = `Leonisaurov/apkc` (push target). `master` must stay identical to upstream; Windows fixes live on branch `windows-support` (pushed to `fork`).

## Build & install

- `go install .` puts the binary at `%USERPROFILE%\go\bin\apkc.exe`. The machine uses this locally-built binary, **never** a GitHub-release one (stock apkc cannot build APKs on Windows).
- There are no unit tests. Verification = end-to-end scaffold build: `printf 'com.x.y\n' | apkc create tmptest && cd tmptest && apkc build`, then:
  - `aapt2 dump badging build/app.apk` must print no `warn:` about missing res paths.
  - `jar tf build/app.apk` — every entry must use `/` (backslashes = zip-name regression).
  - `java -jar "<sdk>/build-tools/36.0.0/lib/apksigner.jar" verify build/app.apk`.
- Do not commit `apkc.exe` (untracked build artifact).

## Windows patch rules

The 5 fixes on `windows-support` (classpath separator via `joinPaths`, d8/apksigner invoked as `java -cp/-jar` instead of exec'ing their `.bat`, pre-creating `build/res/<dir>` before `aapt2 link --output-to-dir`, `filepath.ToSlash` on all zip entries) must stay cross-platform-safe: OS-specific behavior is gated by `runtime.GOOS`, generic improvements (ToSlash, pre-mkdir) applied unconditionally so Linux/macOS flows never regress.

## Non-obvious behavior

- `apkc create` prompts read stdin with a **fresh `bufio.Scanner` per prompt**: the first scanner consumes all piped input, so only the package name is readable non-interactively; app name always falls back to its default. Don't "fix" by adding more piped lines — it won't work.
- `apkc doctor` prints resolved paths without validating existence; `apkc build` needs build-tools and the matching `platforms/android-XX` (major version pairing).
- `R.java` is generated into `src/` every build (by `aapt2 link`, before Kotlin/Java compile). It is intentionally committed in apkc projects; never move it to `build/`.
- `.goreleaser.yml` drives upstream releases; keep `go vet ./...` clean before pushing.
