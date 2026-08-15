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

// Test Impact Analysis (TIA): run only the test classes whose dependencies
// changed since the last run.
//
// Model: static, extension-free. A PHP helper (built-in tokenizer only, no
// pcov/xdebug) reports each file's declared and referenced class FQCNs. From
// that we build a file dependency graph, take the transitive closure from each
// test's directory (Test.php plus its sibling config/Source, so a rule wired in
// config/configured_rule.php counts), and re-run a class only if a file in that
// closure -- or a fixture in its directory -- changed. A file-hash snapshot is
// cached under -tia-cache. Fail-open: on the first run, or any error building
// the graph, everything runs.
//
// Static over-approximates (an imported-but-unused class still forms an edge),
// so TIA may run a few extra tests, but it never skips a real dependency.

type tiaCache struct {
	// Hashes maps a file path to its sha256 at the last snapshot.
	Hashes map[string]string `json:"hashes"`
}

type fileScan struct {
	Declared []string `json:"declared"`
	Refs     []string `json:"refs"`
}

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

func runTIA(classes []testClass, testDirs, srcDirs []string, cacheDir, php, phpunit string, workers int, tmpIsolate bool) int {
	cache := loadCache(cacheDir)
	firstRun := len(cache.Hashes) == 0

	current := hashTree(append(append([]string{}, srcDirs...), testDirs...))
	changed := diffHashes(cache.Hashes, current)

	edges, allFiles, graphOK := buildGraph(append(append([]string{}, srcDirs...), testDirs...), php)

	var impacted []testClass
	for _, c := range classes {
		if firstRun || !graphOK || isImpacted(c.path, edges, allFiles, changed) {
			impacted = append(impacted, c)
		}
	}

	skipped := len(classes) - len(impacted)
	fmt.Printf("TIA: %d changed files, %d impacted, %d skipped\n", len(changed), len(impacted), skipped)

	if len(impacted) == 0 {
		cache.Hashes = current
		saveCache(cacheDir, cache)
		fmt.Println("OK (nothing to run)")
		return 0
	}

	chunks := balance(impacted, workers)
	totalWeight := 0
	for _, c := range impacted {
		totalWeight += c.weight
	}

	failed := run(chunks, php, phpunit, workers, tmpIsolate, totalWeight)

	cache.Hashes = current
	saveCache(cacheDir, cache)

	if failed > 0 {
		fmt.Printf("FAILED chunks: %d\n", failed)
		return 1
	}
	fmt.Println("OK")
	return 0
}

// isImpacted reports whether a test class must run: a fixture (or any file) in
// its own directory changed, or a file in its dependency closure changed.
func isImpacted(testPath string, edges map[string][]string, allFiles []string, changed map[string]bool) bool {
	testDir := filepath.Dir(testPath)
	prefix := testDir + string(os.PathSeparator)

	// any change inside the test's own directory subtree (fixtures, config, Source).
	for path := range changed {
		if path == testPath || strings.HasPrefix(path, prefix) {
			return true
		}
	}

	// seed the closure from every .php file under the test's directory subtree,
	// so a rule referenced only in config/configured_rule.php is reached.
	var seed []string
	for _, file := range allFiles {
		if file == testPath || strings.HasPrefix(file, prefix) {
			seed = append(seed, file)
		}
	}

	// transitive closure over the dependency graph.
	seen := make(map[string]bool)
	queue := seed
	for len(queue) > 0 {
		file := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[file] {
			continue
		}
		seen[file] = true
		if changed[file] {
			return true
		}
		queue = append(queue, edges[file]...)
	}
	return false
}

// buildGraph runs the PHP scanner and returns file->dependency-file edges, the
// list of all scanned .php files (for subtree seeding), and whether it worked.
func buildGraph(dirs []string, php string) (map[string][]string, []string, bool) {
	scanner := writeScanner()
	if scanner == "" {
		return nil, nil, false
	}
	defer os.Remove(scanner)

	args := append([]string{scanner}, dirs...)
	out, err := exec.Command(php, args...).Output()
	if err != nil {
		return nil, nil, false
	}

	var scans map[string]fileScan
	if json.Unmarshal(out, &scans) != nil {
		return nil, nil, false
	}

	// declared FQCN -> file
	declaredToFile := make(map[string]string)
	for file, scan := range scans {
		for _, fqcn := range scan.Declared {
			declaredToFile[fqcn] = file
		}
	}

	edges := make(map[string][]string, len(scans))
	allFiles := make([]string, 0, len(scans))
	for file, scan := range scans {
		allFiles = append(allFiles, file)

		var deps []string
		for _, ref := range scan.Refs {
			if target, ok := declaredToFile[ref]; ok && target != file {
				deps = append(deps, target)
			}
		}
		edges[file] = deps
	}
	return edges, allFiles, true
}

func writeScanner() string {
	f, err := os.CreateTemp("", "fastunit-depscan-*.php")
	if err != nil {
		return ""
	}
	_, _ = f.WriteString(dependencyScanner)
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
	cache := tiaCache{Hashes: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(dir, "tia.json"))
	if err != nil {
		return cache
	}
	_ = json.Unmarshal(data, &cache)
	if cache.Hashes == nil {
		cache.Hashes = map[string]string{}
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
