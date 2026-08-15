// Command fastunit runs a PHPUnit suite in parallel by splitting test classes
// into N balanced "warm" chunks: each worker boots PHP once and runs many test
// classes in a single process, so the container is built N times instead of
// once per class (as tools that spawn a process per chunk do).
//
// Balancing is by fixture count by default, since Rector rule tests iterate one
// assertion per .php.inc fixture, so fixture count approximates a class's
// runtime. Other weighting modes (methods, size) suit generic PHPUnit suites.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type testClass struct {
	path   string
	weight int // min 1
}

var (
	fixtureRe = regexp.MustCompile(`\.php\.inc$`)
	methodRe  = regexp.MustCompile(`(?m)(function\s+test|#\[\s*Test\b)`)
)

func main() {
	workers := flag.Int("p", runtime.NumCPU(), "number of parallel workers")
	php := flag.String("php", "php", "php interpreter")
	// the real PHP entry script (runs cross-platform via `php`); vendor/bin/phpunit
	// is a shell/batch proxy on Windows and cannot be passed to php directly.
	phpunit := flag.String("bin", "vendor/phpunit/phpunit/phpunit", "phpunit entry script")
	weightMode := flag.String("weight", "fixtures", "class weighting: fixtures | methods | size")
	tmpIsolate := flag.Bool("tmp-isolate", true, "give each worker its own TMPDIR (needed for Rector's file cache)")
	tia := flag.Bool("tia", false, "test impact analysis: run only tests whose covered source (or own files) changed")
	srcDirs := flag.String("src", "src,rules", "comma-separated source dirs, for coverage filter and change detection (with -tia)")
	cacheDir := flag.String("tia-cache", ".fastunit-cache", "directory holding the -tia coverage map and file hashes")
	flag.Parse()

	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs = []string{"rules-tests", "tests"}
	}

	classes := discover(dirs, *weightMode)
	if len(classes) == 0 {
		fmt.Fprintln(os.Stderr, "no test classes found")
		os.Exit(1)
	}

	if *tia {
		os.Exit(runTIA(classes, dirs, splitList(*srcDirs), *cacheDir, *php, *phpunit, *workers, *tmpIsolate))
	}

	chunks := balance(classes, *workers)

	totalWeight := 0
	for _, c := range classes {
		totalWeight += c.weight
	}

	start := time.Now()
	failed := run(chunks, *php, *phpunit, *workers, *tmpIsolate, totalWeight)
	elapsed := time.Since(start)

	fmt.Printf("\n%d classes, %d weight (%s), %d chunks, %d workers\n",
		len(classes), totalWeight, *weightMode, len(chunks), *workers)
	fmt.Printf("wall time: %.2fs\n", elapsed.Seconds())

	if failed > 0 {
		fmt.Printf("FAILED chunks: %d\n", failed)
		os.Exit(1)
	}
	fmt.Println("OK")
}

// discover finds *Test.php files and weights each by the chosen mode.
func discover(dirs []string, mode string) []testClass {
	var classes []testClass
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "Test.php") {
				return nil
			}
			classes = append(classes, testClass{path: path, weight: weigh(path, mode)})
			return nil
		})
	}
	return classes
}

func weigh(testPath, mode string) int {
	switch mode {
	case "methods":
		return methodWeight(testPath)
	case "size":
		return sizeWeight(testPath)
	default:
		return fixtureWeight(testPath)
	}
}

// fixtureWeight counts sibling Fixture/ *.php.inc files (Rector convention).
func fixtureWeight(testPath string) int {
	fixtureDir := filepath.Join(filepath.Dir(testPath), "Fixture")
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		return 1
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && fixtureRe.MatchString(e.Name()) {
			count++
		}
	}
	if count < 1 {
		return 1
	}
	return count
}

// methodWeight counts `function test*` and `#[Test]` occurrences.
func methodWeight(testPath string) int {
	data, err := os.ReadFile(testPath)
	if err != nil {
		return 1
	}
	count := len(methodRe.FindAll(data, -1))
	if count < 1 {
		return 1
	}
	return count
}

// sizeWeight weighs by file byte size (in KB, min 1).
func sizeWeight(testPath string) int {
	info, err := os.Stat(testPath)
	if err != nil {
		return 1
	}
	kb := int(info.Size() / 1024)
	if kb < 1 {
		return 1
	}
	return kb
}

// balance greedily packs classes (heaviest first) into n bins, always adding to
// the lightest bin. Minimizes the heaviest chunk, so wall time is bounded by the
// slowest worker rather than by unlucky static splits.
func balance(classes []testClass, n int) [][]testClass {
	sort.Slice(classes, func(i, j int) bool {
		return classes[i].weight > classes[j].weight
	})
	bins := make([][]testClass, n)
	loads := make([]int, n)
	for _, c := range classes {
		min := 0
		for i := 1; i < n; i++ {
			if loads[i] < loads[min] {
				min = i
			}
		}
		bins[min] = append(bins[min], c)
		loads[min] += c.weight
	}
	var out [][]testClass
	for _, b := range bins {
		if len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

func run(chunks [][]testClass, php, phpunit string, workers int, tmpIsolate bool, totalWeight int) int {
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	doneWeight := 0
	doneChunks := 0

	// progress ticks per finished chunk (weight approximates runtime); it cannot
	// go finer, since each worker runs its whole chunk in one warm process.
	report := func() {
		pct := 100
		if totalWeight > 0 {
			pct = doneWeight * 100 / totalWeight
		}
		fmt.Fprintf(os.Stderr, "\rprogress: %3d%% (%d/%d chunks)   ",
			pct, doneChunks, len(chunks))
	}

	for idx, chunk := range chunks {
		wg.Add(1)
		go func(idx int, chunk []testClass) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			chunkWeight := 0
			for _, c := range chunk {
				chunkWeight += c.weight
			}

			// invoke via `php <phpunit>` so it works uniformly on Windows,
			// where vendor/bin/phpunit is not directly executable.
			args := make([]string, 0, len(chunk)+1)
			args = append(args, phpunit)
			for _, c := range chunk {
				args = append(args, c.path)
			}
			cmd := exec.Command(php, args...)

			if tmpIsolate {
				// Each worker gets its own temp dir so Rector's file cache
				// (sys_get_temp_dir()/rector_cached_files) and the fixture temp
				// dumper never race across processes. sys_get_temp_dir() reads
				// TMPDIR on Linux/macOS and TMP/TEMP on Windows, so set all three.
				tmp := filepath.Join(os.TempDir(), fmt.Sprintf("fastunit-%d", idx))
				_ = os.MkdirAll(tmp, 0o755)
				defer os.RemoveAll(tmp)
				cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
			}

			out, err := cmd.CombinedOutput()
			outStr := string(out)

			mu.Lock()
			if err != nil {
				failed++
				// clear the progress line before the failure block.
				fmt.Fprint(os.Stderr, "\r")
				fmt.Printf("chunk FAILED (%d classes): %v\n%s\n", len(chunk), err, tail(outStr, 15))
			} else if hasIssues(outStr) {
				// PHPUnit passed but reported warnings/deprecations/notices; the
				// captured output would otherwise hide them on a green run.
				fmt.Fprint(os.Stderr, "\r")
				fmt.Printf("chunk WARNINGS (%d classes):\n%s\n", len(chunk), issueExcerpt(outStr))
			}
			doneWeight += chunkWeight
			doneChunks++
			report()
			mu.Unlock()
		}(idx, chunk)
	}
	wg.Wait()
	fmt.Fprintln(os.Stderr)
	return failed
}

// hasIssues reports whether a passing PHPUnit run still printed warnings,
// deprecations, notices, or risky tests. PHPUnit prints exactly this phrase in
// that case ("OK, but there were issues!").
func hasIssues(s string) bool {
	return strings.Contains(s, "but there were issues")
}

// issueExcerpt returns the PHPUnit detail block ("There was/were ... warning")
// so the specific warnings surface, not the whole passing-test noise.
func issueExcerpt(s string) string {
	for _, marker := range []string{"There was ", "There were "} {
		if i := strings.Index(s, marker); i >= 0 {
			return strings.TrimRight(s[i:], "\n")
		}
	}
	return tail(s, 20)
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
