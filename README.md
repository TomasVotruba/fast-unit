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

Static, **no coverage driver needed** -- works on any PHP 7.2+ (uses only the
built-in tokenizer, not `pcov`/`xdebug`):

1. A tokenizer-based scanner reads each `.php` file's declared and referenced
   class names and builds a file dependency graph.
2. For each test class, it takes the transitive closure from **every file in the
   test's directory** -- so a rule wired only in `config/configured_rule.php` is
   included -- and re-runs the class only if a file in that closure, or a fixture
   in its directory, changed since the last run.
3. Change detection is a sha256 snapshot cached under `.fastunit-cache/`. The
   graph is rebuilt every run (fast: whole-repo scan is ~1s), so new files are
   always accounted for -- no stale map.

```
TIA: 1 changed files, 1 impacted, 10 skipped   # edited one rule
TIA: 0 changed files, 0 impacted, 688 skipped  # nothing changed -> ~1s
```

Safe by construction: static analysis **over**-approximates (an imported but
unused class still forms an edge), so TIA may run a few extra tests, but it never
skips a real dependency. On the first run (empty cache), or any scan error,
everything runs.

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
