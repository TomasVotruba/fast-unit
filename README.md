# fast-unit

Run a PHPUnit suite **~4x faster** by splitting test classes across parallel
workers that each boot PHP **once** and run many classes in a single process.

Ships as prebuilt Go binaries fetched on first run — **no Go toolchain needed**.
Runs on **PHP 7.2+** (only the tiny downloader shim is PHP; the runner is a
version-agnostic native binary), so it accelerates legacy suites too.

While it runs it prints a live weight-based progress line:

```
progress:  73% (9/12 chunks)
```

Progress ticks per finished chunk (chunk weight approximates runtime). It cannot
go finer, because each worker runs its whole chunk in one warm PHP process --
that single boot is the speedup.

## Install

```bash
composer require --dev tomasvotruba/fast-unit
```

## Usage

```bash
vendor/bin/fastunit -p 12                          # whole suite
vendor/bin/fastunit -p 8 rules-tests/CodeQuality   # a subtree
```

On first run the shim downloads the matching binary for your OS/arch from the
GitHub release, verifies its sha256, caches it, and execs it.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-p` | CPU count | parallel workers |
| `-bin` | `vendor/phpunit/phpunit/phpunit` | phpunit entry script |
| `-weight` | `fixtures` | class weighting: `fixtures` \| `methods` \| `size` |
| `-tmp-isolate` | `true` | give each worker its own `TMPDIR` |
| `-php` | `php` | php interpreter |

Positional args are the directories to scan (default `rules-tests tests`).

### Environment overrides

| Var | Effect |
| --- | --- |
| `FASTUNIT_BINARY=/path` | use this binary, skip all download logic (CI prebuild / airgap) |
| `FASTUNIT_VERSION=vX.Y.Z` | pin a specific release tag |

## Why it is faster

Bootstrap dominates a class's run, not the assertions. A tool that spawns a
fresh process per class pays the container-build cost once per class. fast-unit
splits classes into N chunks balanced by weight, and each worker runs its whole
chunk in one warm process — so the container is built N times, not once per
class.

### Weighting modes

- `fixtures` — count sibling `Fixture/*.php.inc` files. Best for Rector rule
  tests, which iterate one assertion per fixture.
- `methods` — count `function test*` and `#[Test]` occurrences. Good for
  generic PHPUnit suites.
- `size` — file byte size. Cheap fallback.

## Test impact analysis (`-tia`)

Run only the tests whose code actually changed:

```bash
vendor/bin/fastunit -tia rules-tests tests
```

How it works, per class:

1. A per-class PHPUnit coverage run records the **exact set of source files each
   test executes** (needs the `pcov` or `xdebug` driver). That map, plus a
   sha256 snapshot of every source and test file, is cached under
   `.fastunit-cache/`.
2. On the next run, a class is re-run only if its own file, a sibling fixture,
   or one of its covered source files changed. Impacted classes run **with
   coverage again**, so the map self-heals.
3. Fail-open by design: a class with no map entry, or a changed source file that
   no map knows about, runs everything. TIA never silently skips an unknown
   dependency.

```
TIA: 1 changed files, 1 impacted, 10 skipped   # edited one rule
TIA: 0 changed files, 0 impacted, 11 skipped   # nothing changed -> instant
```

Flags: `-src` (comma-separated source dirs, default `src,rules`), `-tia-cache`
(cache dir, default `.fastunit-cache`).

**Add `.fastunit-cache/` to your `.gitignore`** -- it is a local, machine-specific
cache.

## Isolation

With `-tmp-isolate` (default on), each worker gets its own `TMPDIR` so tools
that cache under `sys_get_temp_dir()` (e.g. Rector's `rector_cached_files`) do
not race across parallel processes. Turn it off for suites that do not need it.

> Note: sharing one PHP process across classes can surface latent global-state
> leaks between test classes (static parameter providers, etc.). Reset such
> state in `tearDownAfterClass()`.

## License

MIT
