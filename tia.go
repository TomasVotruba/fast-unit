package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Test Impact Analysis (TIA): run only the test classes whose covered source
// files, own file, or sibling fixtures changed since the last run.
//
// Model: coverage-based, exact. For each test class, a per-class PHPUnit
// coverage run records the set of source files it executes. That map plus a
// file-hash snapshot is cached under -tia-cache. On a later run, a class is
// re-run only if one of its dependencies changed; impacted classes run WITH
// coverage again, so the map self-heals. Fail-open: an unknown class (no map
// entry) or a changed source file absent from every map runs everything.

type tiaCache struct {
	// Hashes maps a repo-relative file path to its sha256 at snapshot time.
	Hashes map[string]string `json:"hashes"`
	// Coverage maps a test file path to the source files it executes.
	Coverage map[string][]string `json:"coverage"`
}

// coverageExtractor is embedded and written to a temp file at run time; it turns
// PHPUnit's serialized --coverage-php dump into a JSON array of covered files.
const coverageExtractor = `<?php
require getcwd() . '/vendor/autoload.php';
$dump = require $argv[1];
$processed = $dump['codeCoverage'];
$files = [];
foreach ($processed->lineCoverage() as $file => $lines) {
    foreach ($lines as $tests) {
        if (is_array($tests) && $tests !== []) {
            $files[$file] = true;
            break;
        }
    }
}
echo json_encode(array_values(array_map(
    static fn (string $f): string => $f,
    array_keys($files)
)));
`

func splitList(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func runTIA(classes []testClass, testDirs, srcDirs []string, cacheDir, php, phpunit string, workers int) int {
	cache := loadCache(cacheDir)

	// current hashes of everything that can invalidate a test: all source files
	// and everything under the test dirs (test classes, fixtures, config).
	current := hashTree(append(append([]string{}, srcDirs...), testDirs...))

	changed := diffHashes(cache.Hashes, current)

	// a changed source file that no map knows about can affect unknown tests;
	// be safe and run everything until the map learns it.
	runAll := len(cache.Coverage) == 0 || changedUnknownSource(changed, cache.Coverage, srcDirs)

	var impacted []testClass
	for _, c := range classes {
		if runAll || isImpacted(c.path, cache.Coverage, changed) {
			impacted = append(impacted, c)
		}
	}

	skipped := len(classes) - len(impacted)
	fmt.Printf("TIA: %d changed files, %d impacted, %d skipped\n", len(changed), len(impacted), skipped)

	if len(impacted) == 0 {
		// still refresh the hash snapshot so deletions/renames settle.
		cache.Hashes = current
		saveCache(cacheDir, cache)
		fmt.Println("OK (nothing to run)")
		return 0
	}

	extractor := writeExtractor()
	defer os.Remove(extractor)

	failed, coverage := runWithCoverage(impacted, srcDirs, php, phpunit, extractor, workers)

	// merge refreshed coverage entries and the new hash snapshot.
	for testPath, files := range coverage {
		cache.Coverage[testPath] = files
	}
	cache.Hashes = current
	saveCache(cacheDir, cache)

	if failed > 0 {
		fmt.Printf("FAILED classes: %d\n", failed)
		return 1
	}
	fmt.Println("OK")
	return 0
}

// isImpacted reports whether a test class must run: its own file, its sibling
// fixtures/config (anything under its directory), or a covered source changed.
func isImpacted(testPath string, coverage map[string][]string, changed map[string]bool) bool {
	covered, known := coverage[testPath]
	if !known {
		return true // fail-open: never seen this class
	}

	testDir := filepath.Dir(testPath)
	for path := range changed {
		if path == testPath || strings.HasPrefix(path, testDir+string(os.PathSeparator)) {
			return true
		}
	}
	for _, src := range covered {
		if changed[src] {
			return true
		}
	}
	return false
}

// changedUnknownSource reports whether any changed source file is absent from
// every coverage entry, meaning its impact cannot be scoped.
func changedUnknownSource(changed map[string]bool, coverage map[string][]string, srcDirs []string) bool {
	known := make(map[string]bool)
	for _, files := range coverage {
		for _, f := range files {
			known[f] = true
		}
	}
	for path := range changed {
		if !underAny(path, srcDirs) {
			continue
		}
		if !known[path] {
			return true
		}
	}
	return false
}

func underAny(path string, dirs []string) bool {
	for _, dir := range dirs {
		if path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// runWithCoverage runs each impacted class in its own PHPUnit process with
// coverage, in parallel, and returns the failure count and refreshed map.
func runWithCoverage(classes []testClass, srcDirs []string, php, phpunit, extractor string, workers int) (int, map[string][]string) {
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	coverage := make(map[string][]string)
	done := 0

	for _, c := range classes {
		wg.Add(1)
		go func(c testClass) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			tmp, err := os.MkdirTemp("", "fastunit-cov-")
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			defer os.RemoveAll(tmp)
			covFile := filepath.Join(tmp, "cov.php")

			// pcov auto-detects a single directory (often just "src"); force the
			// project root so every source dir (e.g. "rules") is instrumented,
			// and exclude vendor to keep collection fast. --coverage-filter still
			// restricts what is recorded to the requested source dirs.
			args := []string{
				"-d", "pcov.enabled=1",
				"-d", "pcov.directory=.",
				"-d", `pcov.exclude=~/(vendor|tests|rules-tests)/~`,
				phpunit,
			}
			for _, dir := range srcDirs {
				args = append(args, "--coverage-filter", dir)
			}
			args = append(args, "--coverage-php", covFile, c.path)

			cmd := exec.Command(php, args...)
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
			out, runErr := cmd.CombinedOutput()

			var covered []string
			if fileExists(covFile) {
				covered = extractCoverage(php, extractor, covFile)
			}

			mu.Lock()
			if runErr != nil {
				failed++
				fmt.Fprint(os.Stderr, "\r")
				fmt.Printf("FAILED %s: %v\n%s\n", c.path, runErr, tail(string(out), 15))
			} else if hasIssues(string(out)) {
				fmt.Fprint(os.Stderr, "\r")
				fmt.Printf("WARNINGS %s:\n%s\n", c.path, issueExcerpt(string(out)))
			}
			if covered != nil {
				coverage[c.path] = covered
			}
			done++
			fmt.Fprintf(os.Stderr, "\rprogress: %3d%% (%d/%d classes)   ", done*100/len(classes), done, len(classes))
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	fmt.Fprintln(os.Stderr)
	return failed, coverage
}

func extractCoverage(php, extractor, covFile string) []string {
	out, err := exec.Command(php, extractor, covFile).Output()
	if err != nil {
		return nil
	}
	var files []string
	if json.Unmarshal(out, &files) != nil {
		return nil
	}
	return files
}

func writeExtractor() string {
	f, err := os.CreateTemp("", "fastunit-extract-*.php")
	if err != nil {
		return ""
	}
	_, _ = f.WriteString(coverageExtractor)
	_ = f.Close()
	return f.Name()
}

// hashTree returns sha256 hashes for every regular .php / .php.inc file under
// the given roots, keyed by path as walked (repo-relative when roots are).
func hashTree(roots []string) map[string]string {
	hashes := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)

	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".php") && !strings.HasSuffix(path, ".php.inc") {
				return nil
			}
			wg.Add(1)
			go func(path string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				sum := hashFile(path)
				mu.Lock()
				hashes[path] = sum
				mu.Unlock()
			}(path)
			return nil
		})
	}
	wg.Wait()
	return hashes
}

func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// diffHashes returns the set of paths whose hash changed, was added, or removed.
func diffHashes(old, current map[string]string) map[string]bool {
	changed := make(map[string]bool)
	for path, sum := range current {
		if old[path] != sum {
			changed[path] = true
		}
	}
	for path := range old {
		if _, ok := current[path]; !ok {
			changed[path] = true // deleted
		}
	}
	return changed
}

func loadCache(dir string) tiaCache {
	cache := tiaCache{Hashes: map[string]string{}, Coverage: map[string][]string{}}
	data, err := os.ReadFile(filepath.Join(dir, "tia.json"))
	if err != nil {
		return cache
	}
	_ = json.Unmarshal(data, &cache)
	if cache.Hashes == nil {
		cache.Hashes = map[string]string{}
	}
	if cache.Coverage == nil {
		cache.Coverage = map[string][]string{}
	}
	return cache
}

func saveCache(dir string, cache tiaCache) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "tia.json"), data, 0o644)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
