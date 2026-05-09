// File: main.go
// Author: Hadi Cahyadi <cumulus13@gmail.com>
// Date: 2026-05-09
// Description: 
// License: MIT

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ──────────────────────────────────────────────
// Data types
// ──────────────────────────────────────────────

type Package struct {
	Name    string
	Version string
}

// Cache stores discovered site-dirs + the fingerprints used to validate them.
type Cache struct {
	Version     int               `json:"version"`
	CreatedAt   int64             `json:"created_at"`
	SiteDirs    []string          `json:"site_dirs"`
	Fingerprint map[string]int64  `json:"fingerprint"` // exe path → mtime unix
}

const cacheVersion = 3

// ──────────────────────────────────────────────
// ANSI helpers
// ──────────────────────────────────────────────

var useColor = true

const (
	cReset  = "\033[0m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cRed    = "\033[31m"
)

func col(code, s string) string {
	if !useColor {
		return s
	}
	return code + s + cReset
}

func highlight(s, query string) string {
	if query == "" || !useColor {
		return s
	}
	lower := strings.ToLower(s)
	lowerQ := strings.ToLower(query)
	idx := strings.Index(lower, lowerQ)
	if idx == -1 {
		return s
	}
	matched := cRed + cBold + s[idx:idx+len(query)] + cReset + cGreen
	return cGreen + s[:idx] + matched + s[idx+len(query):] + cReset
}

// ──────────────────────────────────────────────
// Cache file location
// ──────────────────────────────────────────────

func cacheFilePath() string {
	var base string
	switch runtime.GOOS {
	case "windows":
		if p := os.Getenv("APPDATA"); p != "" {
			base = filepath.Join(p, "piplist")
		}
	case "darwin":
		if home, _ := os.UserHomeDir(); home != "" {
			base = filepath.Join(home, "Library", "Caches", "piplist")
		}
	default:
		// Linux / other: respect XDG
		if p := os.Getenv("XDG_CACHE_HOME"); p != "" {
			base = filepath.Join(p, "piplist")
		} else if home, _ := os.UserHomeDir(); home != "" {
			base = filepath.Join(home, ".cache", "piplist")
		}
	}
	if base == "" {
		base = filepath.Join(os.TempDir(), "piplist")
	}
	return filepath.Join(base, "cache.json")
}

// ──────────────────────────────────────────────
// Cache read / write / validate
// ──────────────────────────────────────────────

func loadCache(path string) (*Cache, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var c Cache
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		return nil, err
	}
	if c.Version != cacheVersion {
		return nil, fmt.Errorf("cache version mismatch")
	}
	return &c, nil
}

func saveCache(path string, c *Cache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

// exeMtime returns the modification time of an executable (unix seconds).
// Returns 0 if the file can't be stat'd.
func exeMtime(exe string) int64 {
	info, err := os.Stat(exe)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

// isCacheValid checks that every fingerprinted exe still has the same mtime,
// and that the cache isn't older than 7 days.
func isCacheValid(c *Cache) bool {
	// Expire after 7 days regardless
	if time.Now().Unix()-c.CreatedAt > 7*24*3600 {
		return false
	}
	for exe, mtime := range c.Fingerprint {
		if exeMtime(exe) != mtime {
			return false
		}
	}
	// Also check that every cached dir still exists
	for _, d := range c.SiteDirs {
		if _, err := os.Stat(d); err != nil {
			return false
		}
	}
	return true
}

// ──────────────────────────────────────────────
// Python discovery
// ──────────────────────────────────────────────

func askPython(exe string) []string {
	script := `import site, sys, os
dirs = set()
try:
    dirs.update(site.getsitepackages())
except Exception:
    pass
try:
    dirs.add(site.getusersitepackages())
except Exception:
    pass
prefix = sys.prefix
for sub in ["site-packages", "dist-packages"]:
    for base in [os.path.join(prefix,"lib"), os.path.join(prefix,"Lib")]:
        p = os.path.join(base, "python%d.%d" % sys.version_info[:2], sub)
        if os.path.isdir(p):
            dirs.add(p)
        p2 = os.path.join(base, sub)
        if os.path.isdir(p2):
            dirs.add(p2)
for d in dirs:
    if os.path.isdir(d):
        print(d)
`
	out, err := exec.Command(exe, "-c", script).Output()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			dirs = append(dirs, line)
		}
	}
	return dirs
}

func pythonBinPaths(root string) []string {
	if runtime.GOOS == "windows" {
		return []string{
			filepath.Join(root, "python.exe"),
			filepath.Join(root, "python3.exe"),
			filepath.Join(root, "Scripts", "python.exe"),
		}
	}
	return []string{
		filepath.Join(root, "bin", "python3"),
		filepath.Join(root, "bin", "python"),
	}
}

func pythonCandidates() []string {
	seenReal := map[string]bool{}
	var result []string

	add := func(p string) {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			real = p
		}
		abs, _ := filepath.Abs(real)
		if !seenReal[abs] {
			seenReal[abs] = true
			result = append(result, p)
		}
	}

	// 1. PATH
	names := []string{
		"python3", "python",
		"python3.13", "python3.12", "python3.11", "python3.10",
		"python3.9", "python3.8", "python3.7", "python2.7",
	}
	if runtime.GOOS == "windows" {
		var wnames []string
		for _, n := range append([]string{"py"}, names...) {
			if !strings.HasSuffix(n, ".exe") {
				wnames = append(wnames, n+".exe")
			} else {
				wnames = append(wnames, n)
			}
		}
		names = wnames
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			add(p)
		}
	}

	homedir, _ := os.UserHomeDir()

	// 2. pyenv
	pyenvRoots := []string{os.Getenv("PYENV_ROOT")}
	if homedir != "" {
		pyenvRoots = append(pyenvRoots, filepath.Join(homedir, ".pyenv"))
	}
	for _, root := range pyenvRoots {
		if root == "" {
			continue
		}
		entries, _ := os.ReadDir(filepath.Join(root, "versions"))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			for _, bin := range pythonBinPaths(filepath.Join(root, "versions", e.Name())) {
				if _, err := os.Stat(bin); err == nil {
					add(bin)
				}
			}
		}
	}

	// 3. conda / mamba
	condaRoots := []string{os.Getenv("CONDA_PREFIX"), os.Getenv("MAMBA_ROOT_PREFIX")}
	if homedir != "" {
		condaRoots = append(condaRoots,
			filepath.Join(homedir, "anaconda3"),
			filepath.Join(homedir, "anaconda"),
			filepath.Join(homedir, "miniconda3"),
			filepath.Join(homedir, "miniconda"),
			filepath.Join(homedir, "mambaforge"),
			filepath.Join(homedir, "miniforge3"),
		)
	}
	condaRoots = append(condaRoots, "/opt/anaconda3", "/opt/miniconda3", "/opt/conda")
	for _, root := range condaRoots {
		if root == "" {
			continue
		}
		for _, bin := range pythonBinPaths(root) {
			if _, err := os.Stat(bin); err == nil {
				add(bin)
			}
		}
		envs, _ := os.ReadDir(filepath.Join(root, "envs"))
		for _, e := range envs {
			if !e.IsDir() {
				continue
			}
			for _, bin := range pythonBinPaths(filepath.Join(root, "envs", e.Name())) {
				if _, err := os.Stat(bin); err == nil {
					add(bin)
				}
			}
		}
	}

	// 4. Homebrew
	brewPrefixes := []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew"}
	if p := os.Getenv("HOMEBREW_PREFIX"); p != "" {
		brewPrefixes = append([]string{p}, brewPrefixes...)
	}
	for _, prefix := range brewPrefixes {
		entries, _ := os.ReadDir(filepath.Join(prefix, "bin"))
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, "python3") || n == "python" || n == "python3" {
				if p := filepath.Join(prefix, "bin", n); func() bool { _, err := os.Stat(p); return err == nil }() {
					add(p)
				}
			}
		}
		opts, _ := os.ReadDir(filepath.Join(prefix, "opt"))
		for _, e := range opts {
			if strings.HasPrefix(e.Name(), "python") {
				for _, bin := range []string{"python3", "python"} {
					p := filepath.Join(prefix, "opt", e.Name(), "bin", bin)
					if _, err := os.Stat(p); err == nil {
						add(p)
					}
				}
			}
		}
	}

	// 5. asdf / mise
	asdfRoots := []string{os.Getenv("ASDF_DIR"), os.Getenv("ASDF_DATA_DIR")}
	if homedir != "" {
		asdfRoots = append(asdfRoots,
			filepath.Join(homedir, ".asdf"),
			filepath.Join(homedir, ".local", "share", "mise", "installs", "python"),
		)
	}
	for _, root := range asdfRoots {
		if root == "" {
			continue
		}
		pyDir := filepath.Join(root, "installs", "python")
		if _, err := os.Stat(pyDir); err != nil {
			pyDir = root
		}
		versions, _ := os.ReadDir(pyDir)
		for _, v := range versions {
			if !v.IsDir() {
				continue
			}
			for _, bin := range pythonBinPaths(filepath.Join(pyDir, v.Name())) {
				if _, err := os.Stat(bin); err == nil {
					add(bin)
				}
			}
		}
	}

	// 6. Active venv / poetry / pipenv
	for _, envVar := range []string{"VIRTUAL_ENV", "PIPENV_ACTIVE", "POETRY_ACTIVE"} {
		if root := os.Getenv(envVar); root != "" {
			for _, bin := range pythonBinPaths(root) {
				if _, err := os.Stat(bin); err == nil {
					add(bin)
				}
			}
		}
	}

	// 7. Windows-specific
	if runtime.GOOS == "windows" {
		for _, drive := range []string{"C:", "D:"} {
			for minor := 13; minor >= 7; minor-- {
				for _, suffix := range []string{"", " (x86)"} {
					root := fmt.Sprintf(`%s\Python3%d%s`, drive, minor, suffix)
					if p := filepath.Join(root, "python.exe"); func() bool { _, e := os.Stat(p); return e == nil }() {
						add(p)
					}
				}
			}
		}
		if homedir != "" {
			waDir := filepath.Join(homedir, "AppData", "Local", "Microsoft", "WindowsApps")
			entries, _ := os.ReadDir(waDir)
			for _, e := range entries {
				n := e.Name()
				if strings.HasPrefix(n, "python") && strings.HasSuffix(n, ".exe") {
					add(filepath.Join(waDir, n))
				}
			}
			scoopDir := filepath.Join(homedir, "scoop", "apps")
			apps, _ := os.ReadDir(scoopDir)
			for _, e := range apps {
				if strings.HasPrefix(e.Name(), "python") {
					if p := filepath.Join(scoopDir, e.Name(), "current", "python.exe"); func() bool { _, err := os.Stat(p); return err == nil }() {
						add(p)
					}
				}
			}
		}
	}

	// 8. Linux system scan
	if runtime.GOOS == "linux" {
		for _, base := range []string{"/usr/bin", "/usr/local/bin", "/opt/bin"} {
			entries, _ := os.ReadDir(base)
			for _, e := range entries {
				n := e.Name()
				if strings.HasPrefix(n, "python3") || strings.HasPrefix(n, "python2") || n == "python" {
					add(filepath.Join(base, n))
				}
			}
		}
	}

	// 9. macOS Xcode / CLT
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Library/Developer/CommandLineTools/usr/bin/python3",
			"/Applications/Xcode.app/Contents/Developer/usr/bin/python3",
		} {
			if _, err := os.Stat(p); err == nil {
				add(p)
			}
		}
	}

	return result
}

// discoverSiteDirs runs full discovery: finds all Pythons, asks each for
// site-packages, returns dirs + the fingerprint map for caching.
func discoverSiteDirs(extraDirs []string) ([]string, map[string]int64) {
	pythons := pythonCandidates()

	var mu sync.Mutex
	seenReal := map[string]bool{}
	var allDirs []string
	fingerprint := map[string]int64{}

	addDir := func(d string) {
		abs, _ := filepath.Abs(d)
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			real = abs
		}
		mu.Lock()
		defer mu.Unlock()
		if !seenReal[real] {
			seenReal[real] = true
			allDirs = append(allDirs, abs)
		}
	}

	var wg sync.WaitGroup
	for _, exe := range pythons {
		exe := exe
		wg.Add(1)
		go func() {
			defer wg.Done()
			dirs := askPython(exe)
			if len(dirs) > 0 {
				mu.Lock()
				fingerprint[exe] = exeMtime(exe)
				mu.Unlock()
				for _, d := range dirs {
					addDir(d)
				}
			}
		}()
	}
	wg.Wait()

	for _, d := range extraDirs {
		addDir(d)
	}
	return allDirs, fingerprint
}

// getSiteDirs returns site-dirs from cache if valid, otherwise runs discovery
// and writes a fresh cache. Prints a status line to stderr if rebuilding.
func getSiteDirs(cachePath string, extraDirs []string, forceRefresh bool, verbose bool) []string {
	if !forceRefresh {
		if c, err := loadCache(cachePath); err == nil && isCacheValid(c) {
			if verbose {
				fmt.Fprintln(os.Stderr, col(cDim, "Using cached site-packages dirs (run --refresh to update):"))
				for _, d := range c.SiteDirs {
					fmt.Fprintln(os.Stderr, col(cDim, "  "+d))
				}
				fmt.Fprintln(os.Stderr)
			}
			return c.SiteDirs
		}
	}

	// Cache miss / invalid — rebuild
	fmt.Fprintln(os.Stderr, col(cYellow, "Discovering Python environments (one-time, result will be cached)..."))
	dirs, fingerprint := discoverSiteDirs(extraDirs)

	if verbose {
		fmt.Fprintln(os.Stderr, col(cDim, "Found site-packages dirs:"))
		for _, d := range dirs {
			fmt.Fprintln(os.Stderr, col(cDim, "  "+d))
		}
		fmt.Fprintln(os.Stderr)
	}

	c := &Cache{
		Version:     cacheVersion,
		CreatedAt:   time.Now().Unix(),
		SiteDirs:    dirs,
		Fingerprint: fingerprint,
	}
	if err := saveCache(cachePath, c); err != nil {
		fmt.Fprintf(os.Stderr, col(cDim, "Warning: could not save cache: %v\n"), err)
	} else {
		fmt.Fprintf(os.Stderr, col(cDim, "Cache saved to: %s\n\n"), cachePath)
	}
	return dirs
}

// ──────────────────────────────────────────────
// Metadata parsing
// ──────────────────────────────────────────────

func parseMetadata(path string) (name, version string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		} else if strings.HasPrefix(line, "Version:") {
			version = strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
		}
		if name != "" && version != "" {
			return name, version, true
		}
	}
	return name, version, name != "" && version != ""
}

func collectPackages(siteDirs []string) []Package {
	seen := map[string]bool{}
	var pkgs []Package
	for _, siteDir := range siteDirs {
		entries, err := os.ReadDir(siteDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dirName := e.Name()
			var metaPath string
			if strings.HasSuffix(dirName, ".dist-info") {
				metaPath = filepath.Join(siteDir, dirName, "METADATA")
			} else if strings.HasSuffix(dirName, ".egg-info") {
				metaPath = filepath.Join(siteDir, dirName, "PKG-INFO")
			} else {
				continue
			}
			name, version, ok := parseMetadata(metaPath)
			if !ok {
				continue
			}
			key := strings.ToLower(name) + "@" + version
			if seen[key] {
				continue
			}
			seen[key] = true
			pkgs = append(pkgs, Package{Name: name, Version: version})
		}
	}
	sort.Slice(pkgs, func(i, j int) bool {
		return strings.ToLower(pkgs[i].Name) < strings.ToLower(pkgs[j].Name)
	})
	return pkgs
}

// ──────────────────────────────────────────────
// Output
// ──────────────────────────────────────────────

func printTable(pkgs []Package, query string) {
	if len(pkgs) == 0 {
		fmt.Println(col(cYellow, "No packages found."))
		return
	}
	maxName, maxVer := len("Package"), len("Version")
	for _, p := range pkgs {
		if len(p.Name) > maxName {
			maxName = len(p.Name)
		}
		if len(p.Version) > maxVer {
			maxVer = len(p.Version)
		}
	}
	sep := strings.Repeat("-", maxName+2) + " " + strings.Repeat("-", maxVer+2)
	fmt.Printf(col(cBold+cCyan, fmt.Sprintf("%-*s  %-*s", maxName, "Package", maxVer, "Version"))+"\n")
	fmt.Println(col(cDim, sep))
	for _, p := range pkgs {
		var name string
		if useColor {
			name = cGreen + highlight(p.Name, query) + cReset
		} else {
			name = p.Name
		}
		ver := col(cYellow, p.Version)
		pad := maxName - len(p.Name)
		fmt.Printf("%s%s  %s\n", name, strings.Repeat(" ", pad), ver)
	}
	fmt.Printf(col(cDim, fmt.Sprintf("\n%d package(s)", len(pkgs)))+"\n")
}

// ──────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────

func main() {
	if runtime.GOOS == "windows" {
		useColor = false
	}

	var (
		query        string
		noColor      bool
		showVer      bool
		extraDirs    string
		verbose      bool
		forceRefresh bool
		showCache    bool
		clearCache   bool
	)

	flag.StringVar(&query, "grep", "", "filter packages by name (case-insensitive)")
	flag.StringVar(&query, "g", "", "filter by name (shorthand)")
	flag.BoolVar(&noColor, "no-color", false, "disable colored output")
	flag.StringVar(&extraDirs, "dir", "", "extra site-packages dirs (comma-separated)")
	flag.BoolVar(&showVer, "version", false, "print version and exit")
	flag.BoolVar(&verbose, "verbose", false, "show scanned site-packages dirs")
	flag.BoolVar(&verbose, "v", false, "show scanned dirs (shorthand)")
	flag.BoolVar(&forceRefresh, "refresh", false, "force re-discover Python envs and update cache")
	flag.BoolVar(&showCache, "cache-path", false, "print cache file location and exit")
	flag.BoolVar(&clearCache, "clear-cache", false, "delete the cache file and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, col(cBold, "piplist")+" v3.0.0 — fast pip package lister\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  piplist                      list all packages\n")
		fmt.Fprintf(os.Stderr, "  piplist <pattern>            filter by name (positional)\n")
		fmt.Fprintf(os.Stderr, "  piplist -g <pattern>         filter by name\n")
		fmt.Fprintf(os.Stderr, "  piplist --no-color           disable colors\n")
		fmt.Fprintf(os.Stderr, "  piplist --dir /a,/b          extra site-packages dirs\n")
		fmt.Fprintf(os.Stderr, "  piplist -v                   show scanned dirs\n")
		fmt.Fprintf(os.Stderr, "  piplist --refresh            force re-discover & update cache\n")
		fmt.Fprintf(os.Stderr, "  piplist --cache-path         show where cache file lives\n")
		fmt.Fprintf(os.Stderr, "  piplist --clear-cache        delete cache file\n")
		fmt.Fprintf(os.Stderr, "  piplist --version            show version\n\n")
		fmt.Fprintf(os.Stderr, "Cache: discovery runs once, result cached for 7 days.\n")
		fmt.Fprintf(os.Stderr, "       Auto-invalidated if any Python executable changes.\n")
		fmt.Fprintf(os.Stderr, "       Run --refresh after installing a new Python/virtualenv.\n\n")
		fmt.Fprintf(os.Stderr, "Auto-discovers: PATH, pyenv, conda/mamba, brew, asdf, mise, virtualenv\n")
	}
	flag.Parse()

	if noColor {
		useColor = false
	}
	if query == "" && flag.NArg() > 0 {
		query = flag.Arg(0)
	}
	if showVer {
		fmt.Println("piplist v3.0.0")
		return
	}

	cachePath := cacheFilePath()

	if showCache {
		fmt.Println(cachePath)
		return
	}
	if clearCache {
		if err := os.Remove(cachePath); err != nil {
			if os.IsNotExist(err) {
				fmt.Println("No cache file found.")
			} else {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Println("Cache cleared:", cachePath)
		}
		return
	}

	var extra []string
	if extraDirs != "" {
		for _, d := range strings.Split(extraDirs, ",") {
			if d = strings.TrimSpace(d); d != "" {
				extra = append(extra, d)
			}
		}
	}

	siteDirs := getSiteDirs(cachePath, extra, forceRefresh, verbose)
	pkgs := collectPackages(siteDirs)

	if query != "" {
		lower := strings.ToLower(query)
		var filtered []Package
		for _, p := range pkgs {
			if strings.Contains(strings.ToLower(p.Name), lower) {
				filtered = append(filtered, p)
			}
		}
		pkgs = filtered
	}

	printTable(pkgs, query)
}
