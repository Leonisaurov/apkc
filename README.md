**apkc** is a bare-bones Android app build system. It lets you build simple native Android apps in APK or AAB format without entire Android IDE. It does a subset of what Android Gradle plugin does. This makes it quick and runs with low resources. apkc is mostly suitable for learning purposes.

## Features

- Base project has zero dependencies. APK is <20 KB.
- Builds base project in <3 seconds.
- Incremental builds and a watch mode.
- Can use external JAR libraries.
- Can use external native shared libs built with C/Go/Rust etc.
- Code completion can be setup in VSCode or other editors.

## Getting started

1. Install JDK and Android SDK and make sure to have `ANDROID_HOME` and `JAVA_HOME` env variables setup.
2. Download `apkc` from Github releases for your OS/ARCH.
3. Use `doctor` command to verify all the paths are correct.

```shell
$ apkc doctor

[doctor] java /usr/local/opt/openjdk@11/ 
[doctor] javac /usr/local/opt/openjdk@11/bin/javac 
[doctor] jarsigner /usr/local/opt/openjdk@11/bin/jarsigner 
[doctor] sdk /Users/ajinasokan/Library/Android/sdk 
[doctor] aapt /Users/ajinasokan/Library/Android/sdk/build-tools/30.0.3/aapt 
[doctor] aapt2 /Users/ajinasokan/Library/Android/sdk/build-tools/30.0.3/aapt2 
[doctor] d8 /Users/ajinasokan/Library/Android/sdk/build-tools/30.0.3/d8 
[doctor] zipalign /Users/ajinasokan/Library/Android/sdk/build-tools/30.0.3/zipalign 
[doctor] android jar /Users/ajinasokan/Library/Android/sdk/platforms/android-30/android.jar
```
4. Create a hello world project.

```shell
$ apkc create myapp

Package name (com.myapp): com.ajinasokan.myapp
App name (My App): Hello World
```

5. Build APK and run in connected device. Logs from app will be streamed after successful run.

```shell
$ cd myapp

$ apkc build

[build] compiling res/layout/main.xml 
[build] compiling res/values/styles.xml 
[build] bundling resources 
[build] compiling java files 
[build] bundling classes and jars 
[build] bundling native libs 
[build] no native libs 
[build] signing apk 
[build] running zipalign 
[apkc] finished in 1.580135483s

$ apkc run

[run] installing in device(serialabcxyz) 
[run] launching main activity 

07-06 11:23:29.894 22390 22390 I libc    : SetHeapTaggingLevel: tag level set to 0
07-06 11:23:29.933 22390 22390 E jinasokan.myap: Not starting debugger since process cannot load the jdwp agent.
....
....
```

To build APK with custom key store run `apkc build --keystore "/path/to/keystore.jks" --storepass "password" --keyalias "alias"`.

To build AAB run `apkc build --aab`. Output file will be at `build/app.{apk/aab}`.

## Incremental builds

`apkc build --inc` keeps the **content** of every tracked file in `build/incremental.json` (sources, resources, the java `aapt` generates, `jar/`, the manifest, `lib/` and `assets/`) and only recompiles what differs by bytes. Nothing in the build decides by timestamp: an editor or a tool that rewrites a file with the same content costs nothing, and new bytes under an old timestamp (a `cp -p`, a `tar -xp`, a sync) cannot hide a stage, which is the failure an incremental build must never ship. Deleting a source or resource also removes the artifacts it produced, so stale classes never stay inside the apk. Switching between `--aab` and apk forces a full rebuild, since the whole output changes shape. An edit of `AndroidManifest.xml` forces a link, and that decision is made on its bytes: putting the same content back is not an edit. The state file names the shape it holds, so a state written by another `apkc` loses its records and produces everything once instead of guessing.

### Dependencies are dexed by bucket, and reach the apk as their own dex files

Above `minSdkVersion` 21, `jar/*.jar` is split into a small number of buckets (ten jars each, six at most) and every bucket is a `d8` call of its own. Its dex files land at `build/dexcache/deps-<sha1>/classes*.dex`, keyed by the sha1 of the api level plus the sha1 of each jar of the bucket, and the bundle writes them into the apk as `classes2.dex`, `classes3.dex`, ... right after the app's `classes.dex`. ART loads them itself at runtime from 21 up, so the app is not merged again with every dependency to produce one big dex.

What that buys: adding a dependency re-dexes the one bucket whose digest changed, not all sixty jars. On the Firebase demo (60 jars, 6 buckets), a whole cold cycle is 4m38s, an idle one 82 ms, one source edit 20s (no bucket redexed), and a single bucket redex is between 39s and 92s depending on how big the bucket is. Before the split, editing a jar cost one pass over all of them (~5 min).

The cache is keyed by content, never by mtime, so `sync` rewriting every jar with the same bytes costs nothing and a jar restored with `cp -p` under an older timestamp cannot hide new bytes from the merge. Two jars with identical bytes count as one input. A bucket that produces no dex (a Kotlin metadata jar such as `androidx.annotation` 1.7.0: `d8` exits 0 without writing anything) is remembered as `deps-<sha1>.nodex` and left out of the next bundle.

Below API 21 the runtime does not open a second dex, so every jar goes back into a single `d8` pass along with the app classes and the output stays `classes.dex`. That makes a dependency an input of the project dex, so below 21 a jar whose bytes changed *does* mean the project has to be dexed again, and the code says so. The `--inc` state's `dexInputs` still holds the whole-set fingerprint (`sha1(api + sha1 of each jar)`), so restoring a bucket whose fingerprint predates a change is enough to force a rebundle even when no bucket itself needs to be re-dexed.

Two honest caveats of the split:

- Above 21, `d8` no longer sees the other jars as inputs when dexing a bucket, and without help it silently **drops** a class whose desugaring needs a type that lives in another bucket (measured on the demo: `androidx.constraintlayout.helper.widget.Carousel` disappeared from the apk, with only a warning "Type ... was not found, it is required for ... desugaring" as the trace). Every `d8` call passes the rest of `jar/` as `--classpath`, so those types resolve without being dexed.
- The single merge used to abort on `Type X is defined multiple times` when two jars defined the same class. Split across buckets, that abort only happens within one bucket; across buckets the same class ships twice and the runtime picks one. `apkc` still keeps its own guard `shadowedClass` for project-vs-dependency duplicates.

### d8 knows the api level

`d8` gets `--min-api` from the manifest's `minSdkVersion`. Without it `d8` assumes API 1 and desugars every lambda and default method of every dependency: on the same project that was 153s for the dex stage instead of 125s, and a bigger dex. A manifest without `uses-sdk` keeps the old behaviour (`d8` default).

`--core-library` is opt-in and off by default. It is the flag a desugared core library jar (the `java.*` ones) needs to dex at all, and with it on `d8` also stops reporting a dependency whose classes collide with the platform, which is why it is not a default. Every `d8` call takes it, the project and the buckets alike: desugaring one stage differently from the other would produce a dex that no single pass would have produced.

### The files aapt generates are compared by content

`aapt2 link --java src` rewrites every generated file on every run even when nothing changed. apkc hashes them before and after the link: identical content is simply not an edit, so a resource whose text changed does not recompile any source; content that really changed invalidates **all** sources, because `javac` inlines `R.*` constants into the class files and a stale class would keep shipping the old id.

Two files are generated, not one: `R.java` for the ids and `Manifest.java`, the old aapt artifact for the `<permission>` elements of the merged manifest, which `aapt2` still writes. Watching only `R.java` left every resource edit recompiling the whole project (measured as 59s instead of 5.5s on a 60 jar project), and `watch` used to treat the rewritten `Manifest.java` under `src/` as a user edit: a round that linked resources triggered one round after itself for nothing.

### `aapt2 link` writes its output directory, it does not own it

`link --output-to-dir` rewrites every file it produces but never removes one, so a resource file deleted (or renamed) in `res/` kept its compiled copy under `build/res/`. The bundle plans every file it finds there, so the drawable shipped inside the apk with no entry in the resource table and no constant in `R.java`, and a rename left both names. `build/res` is emptied right before the link now: it is nothing but that call's output, and an unchanged resource is written again with the same bytes, so the entry reuse is unaffected (measured: a relink of the 60 jar demo still copies 407 entries from the previous package). The observable guarantee is a drawable followed through add, rename and delete: it is in the apk on the first two and in neither the table nor `R.java` after the last.

### The manifest is an input of the link, and of nothing else

`manifestDigest` is a sha1 of the file, so a tool writing the merged manifest again with the same content, a checkout, or a `cp -p` that restores bytes under an old mtime, is not an edit and costs one idle cycle. Before this a touch of an unchanged manifest cost a full rebuild (measured: 62s).

A real edit forces the link, which is what embeds the merged manifest into the package, and nothing else. `aapt2 compile` turns each resource file into a flat before the package name or a resource id exists, so no flat depends on the manifest and not one of them has to be compiled again. What the link rewrites besides the manifest it embeds is `R.java` and `Manifest.java` under `src/`, and those go through the same content comparison as any other generated file, so `javac` runs only when a constant really changed. Measured on the 60 jar demo, a `versionName` bump: 34.7s before (34.3/35.1/34.6, interleaved), because marking every source changed dragged in `javac` (13-15s) and the project `d8` (12-13s) on top of a link that costs under a second; 5.8s, 7.8s and 7.7s after, of which 4.5-6.2s is `apksigner`. Declaring a new permission does rewrite `Manifest.java` and therefore sends the sources through `javac`, still without recompiling a single resource, and going back to the previous bytes costs one idle cycle.

### The api level is an input of the project dex

`--min-api` decides how much of the sources `d8` desugars, so a `minSdkVersion` edit changes `build/classes.dex` even when no source did. The state keeps `appApi`, the api level the project dex was produced with, and any difference re-dexes the project. Without it the edit looks like "only the manifest changed": the fingerprint of `jar/` does move with the api, but above 21 that only rebuilds the package, and the dependency dexes come back from a cache keyed by the new api. Measured with the check removed: returning from api 19 to 21 kept the classes the single merge of api 19 had folded into `classes.dex`, so the dependency shipped in both `classes.dex` and `classes2.dex`, and 21 to 26 dexed the project not at all. Correctness of this stage is therefore a property of `classes.dex` itself, not of what the build log says.

### What the dependencies contribute is decided by their bytes

`build/incremental.json` records the whole-set fingerprint of the jar bytes the last cycle consumed (`dexInputs` = sha1 of `api + sha1 of each jar`), so `jar/` is not tracked by timestamps. A `sync` that rewrites every jar with the same bytes costs nothing (measured: under a second on FirebaseTest, against 59s when the decision was by mtime), and a jar restored with an older timestamp but different bytes cannot escape the merge: the fingerprint differs, `depStale` is true, and the bundle rebuilds. Hashing the 60 jars costs ~98 ms, which is why the check is affordable on every run. Buckets are cached independently by `dexBucket` (dir name is `deps-<sha1>` of that bucket only), so an `add` that moves one jar changes one fingerprint and re-dexes one bucket; the whole-set fingerprint still fires the rebundle.

### What the bundle copies is decided by its roster

`lib/` and `assets/` only reach the apk when the bundle is rebuilt, and until now nothing decided *whether* it had to be: adding a native library left it out of the apk, and deleting one left it inside. The state now keeps `auxInputs`, a digest of every name and byte under those two directories, and any change to that roster forces a bundle. Re-copying the same `.so` over itself stays free, which matters because that is what a dependency tool does on every sync. The digest is affordable even on the worst tree tried here: 136 MiB of assets in 6 files hashes in 0.24s, and an idle cycle on the 60 jar project still finishes in under a second from the shell (82 ms inside apkc).

### A dependency change no longer passes the project through `d8`

Whether the app classes have to be dexed again, and whether the package has to carry other dependency dex files, are two different questions, and until now one flag answered both: any change in `jar/` forced `d8` over the project's own classes as well. So an `add` or a `remove` whose buckets were already cached paid the 8s of a `d8` pass over sources that had not changed. The state separates them: a dependency fingerprint that differs from `dexInputs` rebuilds the package, and only below API 21 (where the jars really are inputs of the one merge) does it also re-dex the project.

Measured on the 60 jar demo, removing a dependency and putting it back with its bucket already cached: 4.9s and 4.86s, of which `apksigner` is 4.6s and 4.7s. Before, each of those cycles also paid the project `d8` (7.97s to 8.52s) and a full package write. What is left there is the JVM that signs the apk, which a debug build cannot skip.

The same split exposed a bug below API 21 that nothing else could see: the fingerprint of `jar/` was only computed inside the bucketed branch, and `jar/` is not tracked by timestamps, so with `minSdkVersion` 19 adding a jar left its classes out of the apk and deleting one left them inside. `jar/*.jar` is now hashed on every incremental run at both api levels (98 ms for 60 jars), so at api 19 an added jar reaches `classes.dex` and a removed one leaves it.

### What `d8` and `javac` warn about is not thrown away

When one of those tools exits 0 its output was discarded. That is exactly the hole a silently dropped class falls through: `androidx.constraintlayout.helper.widget.Carousel` left the apk and the only trace was a warning "Type ... was not found, it is required for ... desugaring". The output of a successful `d8` is now scanned for those two phrases and logged as a warning, and the output of a successful `javac`/`kotlinc` is logged too. Every `d8` call is timed under `[timing]`, including each bucket by name, so a cycle that paid 39s instead of 92s says which bucket did it.

### `lib/` is packed the way the manifest says

`android:extractNativeLibs` is not decoration: `"false"` promises the runtime can map a `.so` straight out of the apk, which only holds when the entry is **stored** (not deflated) and page aligned, while `"true"` — or the attribute being absent — means the package manager unpacks the libraries at install. apkc reads the attribute and packs `lib/` accordingly, asking `zipalign -p` for the page alignment when the libraries are stored. The round says which of the two it did, because the wrong pairing installs and then fails to load the library. Flipping the attribute rewrites those entries even though not one byte of any `.so` moved: an entry is recorded by its bytes *and* its method, and the roster of `lib/` carries the method too.

### Kotlin takes the whole module

`kotlinc` has no per-file incremental mode at the command line, so `--inc` decides **whether** the stage runs, not how much of the module it reads: when a `.kt` moved, the module goes in, with the `.java` sources listed beside it because Kotlin resolves against them without compiling them. Handing over the directory `src` *and* the changed files asked for the same sources twice. Only the `.kt` files are recorded — recording the `.java` here would hide them from `javac`, which runs after.

A project with `.kt` files needs two things the Android SDK does not ship: the compiler on `PATH`, and a `kotlin-stdlib*.jar` in `jar/`, because `android.jar` carries no `kotlin.*`. The build names whichever is missing instead of dying with an empty error or a wall of unresolved references.

### What is still not tracked: who calls whom

Changing the signature of a method in one file does **not** recompile the callers. `javac` is handed only the files whose content moved, and it trusts a `.class` newer than its source, so the build stays green and the apk carries a caller linked against a descriptor that no longer exists: `NoSuchMethodError` on the device. `-sourcepath` was tried and does not help, because javac only pulls in a source when the `.java` is newer than its `.class` — which a timestamp decision cannot answer either. Closing this needs a real dependency graph: which project class references which, read from the constant pool of the compiled classes, so a changed class invalidates the sources that name it. Until then, an API change is a reason to run `apkc build` without `--inc`.

### The package can be aligned and signed without a JVM

`--signer=go` (accepted by `build`, `run` and `watch`) writes `build/app.apk` from the plan with the alignment of every entry decided as it is written, and appends an APK Signing Block holding an APK Signature Scheme v2 signature computed here: `zipalign` and `apksigner` never start, and the package is not rewritten a second time to move its uncompressed entries onto a boundary. Measured on a demo with one source edited per round, three alternating pairs: 7.39 s, 7.42 s and 7.85 s with the JVM tools against 5.52 s, 5.60 s and 5.62 s with the Go packager, and `apksigner verify --verbose` reports `Verified using v2 scheme: true` for every one of them. The default stays `--signer=jvm` until this path has been installed on a phone.

What the Go packager does not do: JAR signing (v1). The v2 scheme is checked from Android 7 up, so `--signer=go` refuses a project whose `minSdkVersion` is below 24 rather than produce an apk that installs on nothing, and says so before writing anything. `--aab` keeps the JVM path, since a bundle is assembled by `bundletool` and signed by `jarsigner`. The key comes straight from the PKCS#12 keystore — JKS and the pre-PBKDF2 encryption schemes are refused by name, with `--signer=jvm` in the message, because a second key derivation that cannot be tested here is not something a signing tool should guess at.

### One state file, written once, and atomically

`build/incremental.json` tracks 410 files on the demo project, and nine places in the code used to save it, each one rewriting the whole JSON. It is written once, at the end of an incremental build, through `build/incremental.json.tmp` and a rename, so a build that dies cannot leave a half written state file behind. Two checkpoints stay: right after the resource stage and right after the compilers, because a build that aborts later (a duplicated class, a failing `d8`) has already produced classes that the next cycle must know about, or the tree keeps an orphan `.class` that no later run can prune. An idle cycle writes the file zero times.

### A project with no sources is idle

"App dex missing" used to mean `build/classes.dex` is not on disk, which a project with an empty `src/` answers on every run: it re-bundled, realigned and signed an apk forever. It now means the dex is missing *and* there are compiled classes to put in it.

### The package hands over the entries that did not change

Building the package meant writing every entry again, so a project with 136 MiB of `assets/` recompressed all of them to ship one edited class file. The bundle now plans its entries first (manifest, the app dex, the dependency dexes, resources, `res/`, `assets/`, `lib/`), and the state records the sha1 of the file that produced each one. When the plan matches the package already on disk and a source file hashes to what it was, that entry is copied over with `zip.Writer.Copy`, which moves the stored bytes without inflating or re-deflating them, method and name intact (so `resources.arsc` keeps being stored, not compressed). Anything new or changed is written as before.

Measured on the 142 MB demo (136 MiB of model assets, one source edited per cycle): the package stage went from 2.886s with the reuse turned off to 0.483s, 0.487s and 0.525s with it on, and the apk comes out byte for byte the same size either way. The package stays deterministic for a given content, which is what makes the next section affordable.

### Signing is decided by the bytes of the package

`zipalign` and `apksigner` are two JVMs, and they used to run whenever a bundle was built. The state keeps the sha1 of `build/bundle.zip` and skips both when those bytes are what the apk on disk was made from. A build without `--inc` still always writes the apk: callers of the plain build check that the file got newer to know it ran, so that path is not gated, and under `--inc` every input that changes content changes a dex, a resource or an asset, and therefore the package sha. What is deliberately not rewritten is an input whose bytes do not reach the package at all, like a comment in a source file that `javac` does not record.

One signing option was tried and reverted: `--align-file-size=false` avoids padding every uncompressed entry to a 4K boundary, which on a large package looked like the cheaper job. It is not. On the 142 MB demo it cost 11.09s and 13.04s against 8.24s with the default, and the package came out bigger. Repeated as four interleaved pairs on the 3.2 MB demo, so that both forms were measured against the same thermal state, every pair was slower with the flag (mean 4.378s against 4.079s) and 3969 bytes larger. The padding is also what lets the runtime map an uncompressed entry directly, so the default stands.

### Measured on a real project

60 dependency jars, 15 sources, 410 tracked files, `tcr -j 1`, one thing changed per run — with the split into six buckets, the apk carries `classes.dex` (project) plus `classes2.dex` … `classes7.dex` (buckets).

| change | before the split | now |
|---|---|---|
| nothing (idle) | ~2s | **~0.1s inside apkc**, apk untouched, state file not rewritten |
| one source file | 3m56s | **18.5s** (javac 5.9s, `d8` app 8.0s, package 0.1s, apksigner 3.9s) — no bucket re-dexed |
| the same file touched, bytes unchanged | 3m56s | **idle**: no compile, no package, apk left exactly as it was |
| a resource whose ids did not change | 3m56s | **5.5s** |
| every jar rewritten with the same bytes | 59s | **under 1s** |
| one bucket lost (its jars' bytes changed) | one pass over all of them | **one `d8` call**, 23.6s on a small bucket, 31.6s on the same one hot, 92s measured on a large one |
| a dependency removed, and put back with its bucket cached | paid the project `d8` as well | **4.9s and 4.86s**, of which signing is 4.6s and 4.7s |
| the manifest touched, same bytes | 62s | **under 0.5s**, apk untouched |
| a real manifest edit (`versionName`) | 74s | **5.8 / 7.8 / 7.7s** (link 0.6-0.8s, `apksigner` 4.5-6.2s); no resource and no source recompiled |
| every source touched | 71s | **60s** |
| a source deleted and put back | 49s + 82s | **30s + 34s** |
| a jar replaced by `cp -p` (new bytes, old mtime) | **it did not notice**: the apk kept the dex of the jar it replaced | the fingerprint changes, the bucket of that jar re-dexes alone, the apk carries the new bytes |
| a jar added or removed below `minSdkVersion` 21 | **it did not notice either**: the classes stayed out of `classes.dex`, or stayed in | the project re-dexes (correct, the jars are inputs of that merge) and the classes go in and out |
| first cold build (all 6 buckets) | ~11m24s to warm the cache | **4m38s** |

These are one device heating up, so the same cycle measured 18.5s and 28.3s on two runs ten minutes apart, and `javac` on the bigger demo moved between 8.4s and 11.9s for identical work. Read the rows as ranges, and read `[timing]` on your own project before believing any single number.

The last two rows are the ones that matter: an incremental build that answers fast and ships the wrong bytes is worse than a slow one. Every stage decides by content now — the sources, the resources, the java `aapt` generates, `jar/`, the manifest, `lib/` and `assets/` — with a sha1 of those bytes, so a timestamp can neither force work that is not needed nor hide work that is. What still costs a full pass does so on purpose: a new resource id invalidates every source (`javac` inlines `R.*` into the class files), and a `minSdkVersion` change invalidates every compiled constant at the new api level, the dependency buckets included. A manifest edit that changes neither pays only the link and the signature.

Building the six buckets the first time costs 4m38s; `apkc clean` deletes them with `build/`. The first run after upgrading is one full rebuild, because the old state file has neither `dexInputs` nor `manifestDigest` yet and the per-jar cache of the previous layout is not a bucket.

What is left when a source changes is `d8` on the project plus the package and the signature, ~18.5s measured. It does not get cheaper by pre-merging the six bucket dexes into one file: measured before, merging 49 separate dex files plus the app classes took 53s, pre-merging the 49 into a single dex took 36s once and then 40s to merge that one with the app classes. The cost is in producing the multi-MB output dex, not in the number of inputs.

The one remaining cut on a source edit would be bucketing the project's own classes the way `jar/` is bucketed, so that only the bucket holding the edited file is dexed. Measured instead of built: `d8` over the project takes 7.97s to 8.52s on the 60 jar demo and 12.9s to 13.9s on the 142 MB one, where the project has 20 classes. A bucket that contains the edited class pays that JVM anyway, so what could be saved is the *other* buckets' share, and at these sizes that is a couple of seconds at best. Against that, the staleness hole is real and subtle: a class whose bytes did not change but whose desugaring depends on the *shape* of a class that did change, in another bucket, would keep its old dex and fail at runtime with no signal in the build. So the project is not bucketed, and the floor of a source edit stays what `d8`, `zipalign` and `apksigner` cost.

Every stage that took more than 100 ms logs its time under `[timing]`, so a slow cycle can be blamed on a stage instead of on the total. When a stage runs that a reader would not expect, `[Incremental]` says why: how many files `javac` is about to recompile, and which generated file `aapt2` produced, changed or stopped producing.

`apkc clean` deletes the `build/` directory together with its incremental state and the bucket dex cache under `build/dexcache/`.

## Watching for changes

```shell
$ apkc watch --inc             # rebuild on every saved change
$ apkc watch --inc --run       # and install + follow the app after each rebuild
$ apkc watch --inc --run --rish # install through the Shizuku shell instead of adb
```

`watch` starts one `apkc` process per round, so a build that fails leaves the watcher running and waiting for the next change, and every round logs that it started — without that line the log ends right after "a file changed", which looks exactly like a watcher that lost the event. Without `--run` it never touches the device, and a plain build is never killed halfway: changes that land while a round runs are coalesced into one round after it, not replayed one per queued event. With `--run` the round replaces the running app at once, and it is killed by process **group**, because the round's `adb logcat` or `rish` grandchild shares the terminal and would otherwise keep streaming over the next round. The `build/` directory, dot directories and the generated `R.java` and `Manifest.java` are ignored, because they are apkc output.

On Android/Termux this relies on inotify, which reports changes for projects under `$HOME` (Termux private storage). Projects on shared storage (`/sdcard`) do not deliver events.

## Running without adb

`apkc run --rish` installs and follows the app through [Shizuku](https://shizuku.rikka.app/)'s shell instead of the Android SDK platform tools. The apk is piped through the shell's stdin into `/data/local/tmp` and installed with `pm install -r`, so app data survives each round and Termux does not need the shared storage permission. If `rish` is not available the run stops there and says so.

One caveat worth knowing: `rish` exits 0 whatever the device command did, so apkc reads the reply text and aborts on a `Failure` from `pm install` or an `Error` from `am start` instead of reporting a successful run. `adb` answers the same way about the launch: `am start -W` reports its outcome in the text.

### The log of an app that died before it could be followed

Both backends used to gate the log on a live pid: an app that crashes in `onCreate` has no pid by the time `pidof` runs, and the user got "couldn't get the pid" while the `FATAL EXCEPTION` that explained it stayed in the device buffer. `run` now takes a timestamp of the device clock **before** the launch (it does not clear the buffer), and asks for the app's log whatever `pidof` answers: the crash buffer first, then the lines of the main buffer that belong to the package — by its live pid, by the pid the `ActivityManager` names next to it, or by continuing one of those records, which is what keeps a stack trace readable. The same report runs when the stream ends and the process is gone, which is the case `watch --run` hits constantly: the round that follows installs over the app, so without it a crash between rounds was simply lost. If the app is not there, the run says so and prints what the device still holds; if the buffer holds nothing, it says that too.

## Verifying

```shell
$ go test ./...                     # incremental state logic
```

You can add external jar dependencies to `myapp/jar` directory and native libs to `myapp/lib` in appropriate architecture sub directories.

You can add [raw assets](https://developer.android.com/guide/topics/resources/providing-resources#OriginalFiles) to `myapp/assets` directory.

## Code completion in VSCode

1. Install [Java extension](https://marketplace.visualstudio.com/items?itemName=redhat.java)
2. Add `android.jar` path from `doctor` command output to `referencedLibraries` list of your workspace VSCode settings file. `myapp/.vscode/settings.json`

```json
{
    "java.project.referencedLibraries": [
        "<pathtosdk>/platforms/android-30/android.jar"
    ]
}
```
3. You could also add other external jar file dependencies

## Backstory

apkc was born in 2016 as a bash script for some of [my experiments](https://ajinasokan.com/posts/smallest-app/). It then evolved into a Makefile and was used for production builds of one of [my apps](https://play.google.com/store/apps/details?id=com.innoventionist.artham). Since the Play Store started enforcing AAB files for publishing I had to move away from apkc. This project is a rewrite of the Makefile. Incremental compilation was added later in this fork, together with the watch mode and the rish runner.