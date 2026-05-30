// File: main.go
// Author: Hadi Cahyadi <cumulus13@gmail.com>
// Description: Fast pip package lister — reads metadata directly from disk
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

const version = "3.2.1"

// ──────────────────────────────────────────────
// Data types
// ──────────────────────────────────────────────

type Package struct {
	Name     string
	Version  string
	Location string // populated only when showLocation=true
}

type Cache struct {
	Version     int              `json:"version"`
	CreatedAt   int64            `json:"created_at"`
	SiteDirs    []string         `json:"site_dirs"`
	RootPaths   []string         `json:"root_paths"`
	Fingerprint map[string]int64 `json:"fingerprint"`
}

const cacheVersion = 4

// cacheTTLSeconds is the cache expiry duration. Set from config at startup;
// defaults to 7 days. Do NOT access before loadConfig() runs in main().
var cacheTTLSeconds int64 = 7 * 24 * 3600

// ──────────────────────────────────────────────
// TTY + color detection
// ──────────────────────────────────────────────

var useColor = false // set by initColor()

// initColor decides whether to emit ANSI codes.
// Priority (highest first):
//  1. --no-color flag / NO_COLOR env        → always off
//  2. --color flag / CLICOLOR_FORCE env      → always on
//  3. stdout is not a TTY                    → off
//  4. TERM=dumb                              → off
//  5. otherwise                              → on
func initColor(forceOn, forceOff bool) {
	if forceOff || os.Getenv("NO_COLOR") != "" {
		useColor = false
		return
	}
	if forceOn || os.Getenv("CLICOLOR_FORCE") != "" {
		useColor = true
		return
	}
	if !isTerminalFd(os.Stdout.Fd()) {
		useColor = false
		return
	}
	if os.Getenv("TERM") == "dumb" {
		useColor = false
		return
	}
	useColor = true
}

// ──────────────────────────────────────────────
// ANSI helpers
// ──────────────────────────────────────────────

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
	cRed    = "\033[31m"
)

// 24-bit truecolor codes for path columns.
const (
	cPathDefault = "\033[38;2;0;255;255m"  // #00FFFF — inside default Python lib root
	cPathCustom  = "\033[38;2;255;255;0m"  // #FFFF00 — editable / outside default root
)

func col(code, s string) string {
	if !useColor {
		return s
	}
	return code + s + cReset
}

// highlight wraps the matched portion of s using configurable colorTable colors.
// When query is empty the whole name gets the name color.
func highlight(s, query string) string {
	if !useColor {
		return s
	}
	nameC := colorTable.name
	if nameC == "" {
		nameC = cGreen
	}
	hlC := colorTable.highlight
	if hlC == "" {
		hlC = cRed + cBold
	}
	if query == "" {
		return nameC + s + cReset
	}
	lower := strings.ToLower(s)
	lowerQ := strings.ToLower(query)
	idx := strings.Index(lower, lowerQ)
	if idx == -1 {
		return nameC + s + cReset
	}
	return nameC + s[:idx] +
		hlC + s[idx:idx+len(query)] + cReset +
		nameC + s[idx+len(query):] + cReset
}

// isDefaultPath reports whether loc lives inside one of the known Python lib roots.
func isDefaultPath(loc string, defaultRoots []string) bool {
	absLoc, err := filepath.Abs(loc)
	if err != nil {
		absLoc = loc
	}
	for _, root := range defaultRoots {
		if strings.HasPrefix(absLoc, root) {
			return true
		}
	}
	return false
}

// pathColor returns the truecolor code appropriate for a Location string.
// Paths under a known Python lib root → #00FFFF; everything else → #FFFF00.
func pathColor(loc string, defaultRoots []string) string {
	if isDefaultPath(loc, defaultRoots) {
		return cPathDefault
	}
	return cPathCustom
}

// ──────────────────────────────────────────────
// Cache helpers
// ──────────────────────────────────────────────

// winUserDataDir returns the best Windows user data directory, immune to
// corrupted env vars (e.g. APPDATA="C:\Program Files\PowerShell\7;C:\Users\...").
//
// Strategy — try each source in order, validate strictly, return first winner:
//  1. USERPROFILE\AppData\Roaming  — constructed, not read raw from APPDATA
//  2. USERPROFILE\AppData\Local    — fallback
//  3. os.TempDir()                  — last resort
//
// We derive paths from USERPROFILE (a simple home dir, rarely corrupted) rather
// than reading APPDATA directly, because APPDATA is the one that gets stomped
// by PowerShell / other installers prepending to it like a PATH variable.
func winUserDataDir() string {
	// Prefer constructing from USERPROFILE — split on ";" and take first
	// token that is an absolute path to an existing directory.
	userProfile := ""
	for _, tok := range strings.Split(os.Getenv("USERPROFILE"), ";") {
		tok = strings.TrimSpace(tok)
		if tok == "" || !filepath.IsAbs(tok) {
			continue
		}
		if fi, err := os.Stat(tok); err == nil && fi.IsDir() {
			userProfile = tok
			break
		}
	}

	if userProfile != "" {
		// Try %USERPROFILE%\AppData\Roaming first (equivalent to %APPDATA%)
		roaming := filepath.Join(userProfile, "AppData", "Roaming")
		if fi, err := os.Stat(roaming); err == nil && fi.IsDir() {
			return roaming
		}
		// Then %USERPROFILE%\AppData\Local (equivalent to %LOCALAPPDATA%)
		local := filepath.Join(userProfile, "AppData", "Local")
		if fi, err := os.Stat(local); err == nil && fi.IsDir() {
			return local
		}
		// Bare USERPROFILE itself
		return userProfile
	}

	// Last resort: os.TempDir() is always valid
	return os.TempDir()
}

func cacheFilePath() string {
	var base string
	switch runtime.GOOS {
	case "windows":
		// Always derive from USERPROFILE, never from APPDATA directly.
		// APPDATA is frequently corrupted on developer machines (PowerShell,
		// conda, other tools prepend to it like a PATH variable).
		base = filepath.Join(winUserDataDir(), "piplist")
	case "darwin":
		if home, _ := os.UserHomeDir(); home != "" {
			base = filepath.Join(home, "Library", "Caches", "piplist")
		}
	default:
		if p := os.Getenv("XDG_CACHE_HOME"); p != "" && filepath.IsAbs(p) {
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
		return nil, fmt.Errorf("cache version mismatch (want %d, got %d)", cacheVersion, c.Version)
	}
	return &c, nil
}

func saveCache(path string, c *Cache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(c); encErr != nil {
		f.Close()
		os.Remove(tmp)
		return encErr
	}
	f.Close()
	return os.Rename(tmp, path)
}

func exeMtime(exe string) int64 {
	info, err := os.Stat(exe)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

func isCacheValid(c *Cache) bool {
	if time.Now().Unix()-c.CreatedAt > cacheTTLSeconds {
		return false
	}
	for exe, mtime := range c.Fingerprint {
		if exeMtime(exe) != mtime {
			return false
		}
	}
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

type pythonSiteInfo struct {
	siteDirs []string
	libRoots []string
}

func askPython(exe string) *pythonSiteInfo {
	script := `import site, sys, os, json
dirs = set()
roots = set()

def add(d):
    if os.path.isdir(d):
        dirs.add(d)
        roots.add(os.path.dirname(d))

try:
    for d in site.getsitepackages():
        add(d)
except Exception:
    pass
try:
    add(site.getusersitepackages())
except Exception:
    pass

prefix = sys.prefix
for sub in ("site-packages", "dist-packages"):
    for base in (
        os.path.join(prefix, "lib"),
        os.path.join(prefix, "Lib"),
    ):
        for d in (
            os.path.join(base, "python%d.%d" % sys.version_info[:2], sub),
            os.path.join(base, sub),
        ):
            add(d)

print(json.dumps({"dirs": sorted(dirs), "roots": sorted(roots)}))
`
	out, err := exec.Command(exe, "-c", script).Output()
	if err != nil {
		return nil
	}
	var result struct {
		Dirs  []string `json:"dirs"`
		Roots []string `json:"roots"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &result); err != nil {
		return nil
	}
	if len(result.Dirs) == 0 {
		return nil
	}
	return &pythonSiteInfo{siteDirs: result.Dirs, libRoots: result.Roots}
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

	// pyenv
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

	// conda / mamba
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

	// Homebrew
	brewPrefixes := []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew"}
	if p := os.Getenv("HOMEBREW_PREFIX"); p != "" {
		brewPrefixes = append([]string{p}, brewPrefixes...)
	}
	for _, prefix := range brewPrefixes {
		entries, _ := os.ReadDir(filepath.Join(prefix, "bin"))
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, "python3") || n == "python" || n == "python3" {
				p := filepath.Join(prefix, "bin", n)
				if _, err := os.Stat(p); err == nil {
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

	// asdf / mise
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

	// Active venv / poetry / pipenv
	for _, envVar := range []string{"VIRTUAL_ENV", "PIPENV_ACTIVE", "POETRY_ACTIVE"} {
		if root := os.Getenv(envVar); root != "" {
			for _, bin := range pythonBinPaths(root) {
				if _, err := os.Stat(bin); err == nil {
					add(bin)
				}
			}
		}
	}

	// Windows-specific
	if runtime.GOOS == "windows" {
		for _, drive := range []string{"C:", "D:"} {
			for minor := 13; minor >= 7; minor-- {
				for _, suffix := range []string{"", " (x86)"} {
					root := fmt.Sprintf(`%s\Python3%d%s`, drive, minor, suffix)
					p := filepath.Join(root, "python.exe")
					if _, err := os.Stat(p); err == nil {
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
					p := filepath.Join(scoopDir, e.Name(), "current", "python.exe")
					if _, err := os.Stat(p); err == nil {
						add(p)
					}
				}
			}
		}
	}

	// Linux system scan
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

	// macOS Xcode / CLT
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

func discoverEnvs(extraDirs []string) (siteDirs []string, libRoots []string, fingerprint map[string]int64) {
	pythons := pythonCandidates()

	var mu sync.Mutex
	seenSite := map[string]bool{}
	seenRoot := map[string]bool{}
	fingerprint = map[string]int64{}

	addDir := func(d string) {
		abs, _ := filepath.Abs(d)
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			real = abs
		}
		if !seenSite[real] {
			seenSite[real] = true
			siteDirs = append(siteDirs, abs)
		}
	}
	addRoot := func(r string) {
		abs, _ := filepath.Abs(r)
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			real = abs
		}
		if !seenRoot[real] {
			seenRoot[real] = true
			libRoots = append(libRoots, abs)
		}
	}

	var wg sync.WaitGroup
	for _, exe := range pythons {
		exe := exe
		wg.Add(1)
		go func() {
			defer wg.Done()
			info := askPython(exe)
			if info == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			fingerprint[exe] = exeMtime(exe)
			for _, d := range info.siteDirs {
				addDir(d)
			}
			for _, r := range info.libRoots {
				addRoot(r)
			}
		}()
	}
	wg.Wait()

	for _, d := range extraDirs {
		addDir(d)
	}
	return
}

func getSiteEnv(cachePath string, extraDirs []string, forceRefresh, verbose bool) ([]string, []string) {
	if !forceRefresh {
		if c, err := loadCache(cachePath); err == nil && isCacheValid(c) {
			if verbose {
				fmt.Fprintln(os.Stderr, col(cDim, "Using cached site-packages dirs (run --refresh to update):"))
				for _, d := range c.SiteDirs {
					fmt.Fprintln(os.Stderr, col(cDim, "  "+d))
				}
				if len(c.RootPaths) > 0 {
					fmt.Fprintln(os.Stderr, col(cDim, "Python lib roots:"))
					for _, r := range c.RootPaths {
						fmt.Fprintln(os.Stderr, col(cDim, "  "+r))
					}
				}
				fmt.Fprintln(os.Stderr)
			}
			return c.SiteDirs, c.RootPaths
		}
	}

	fmt.Fprintln(os.Stderr, col(cYellow, "Discovering Python environments (one-time, result will be cached)..."))
	siteDirs, libRoots, fp := discoverEnvs(extraDirs)

	if verbose {
		fmt.Fprintln(os.Stderr, col(cDim, "Found site-packages dirs:"))
		for _, d := range siteDirs {
			fmt.Fprintln(os.Stderr, col(cDim, "  "+d))
		}
		if len(libRoots) > 0 {
			fmt.Fprintln(os.Stderr, col(cDim, "Python lib roots:"))
			for _, r := range libRoots {
				fmt.Fprintln(os.Stderr, col(cDim, "  "+r))
			}
		}
		fmt.Fprintln(os.Stderr)
	}

	c := &Cache{
		Version:     cacheVersion,
		CreatedAt:   time.Now().Unix(),
		SiteDirs:    siteDirs,
		RootPaths:   libRoots,
		Fingerprint: fp,
	}
	if err := saveCache(cachePath, c); err != nil {
		fmt.Fprintf(os.Stderr, col(cDim, "Warning: could not save cache: %v\n"), err)
	} else {
		fmt.Fprintf(os.Stderr, col(cDim, "Cache saved to: %s\n\n"), cachePath)
	}
	return siteDirs, libRoots
}

// ──────────────────────────────────────────────
// Metadata + package collection
// ──────────────────────────────────────────────

func parseMetadata(path string) (name, ver string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if name == "" && strings.HasPrefix(line, "Name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		} else if ver == "" && strings.HasPrefix(line, "Version:") {
			ver = strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
		}
		if name != "" && ver != "" {
			return name, ver, true
		}
	}
	return name, ver, name != "" && ver != ""
}

// locationFromSiteDir is called ONLY when showLocation=true.
// Returns project path for editable installs, site-dir otherwise.
func locationFromSiteDir(siteDir, distInfoDir string) string {
	duPath := filepath.Join(siteDir, distInfoDir, "direct_url.json")
	data, err := os.ReadFile(duPath)
	if err == nil {
		var du struct {
			URL     string `json:"url"`
			DirInfo struct {
				Editable bool `json:"editable"`
			} `json:"dir_info"`
		}
		if json.Unmarshal(data, &du) == nil && du.DirInfo.Editable {
			loc := strings.TrimPrefix(du.URL, "file://")
			if runtime.GOOS == "windows" && strings.HasPrefix(loc, "/") {
				loc = loc[1:]
			}
			if loc != "" {
				return filepath.FromSlash(loc)
			}
		}
	}
	return siteDir
}

func collectPackages(siteDirs []string, showLocation bool) []Package {
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
			var metaPath, distInfoDir string
			switch {
			case strings.HasSuffix(dirName, ".dist-info"):
				metaPath = filepath.Join(siteDir, dirName, "METADATA")
				distInfoDir = dirName
			case strings.HasSuffix(dirName, ".egg-info"):
				metaPath = filepath.Join(siteDir, dirName, "PKG-INFO")
				distInfoDir = dirName
			default:
				continue
			}
			name, ver, ok := parseMetadata(metaPath)
			if !ok {
				continue
			}
			key := strings.ToLower(name) + "@" + ver
			if seen[key] {
				continue
			}
			seen[key] = true
			var loc string
			if showLocation {
				loc = locationFromSiteDir(siteDir, distInfoDir)
			}
			pkgs = append(pkgs, Package{Name: name, Version: ver, Location: loc})
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

func printRootPaths(roots []string) {
	if len(roots) == 0 {
		fmt.Println(col(cYellow, "No Python lib root paths found."))
		return
	}
	fmt.Println(col(cBold+cCyan, "Python lib root path(s):"))
	sep := strings.Repeat("─", 50)
	fmt.Println(col(cDim, sep))
	for _, r := range roots {
		if useColor {
			fmt.Println(cPathDefault + "  " + r + cReset)
		} else {
			fmt.Println("  " + r)
		}
	}
	fmt.Println(col(cDim, sep))
	fmt.Println()
}

func printTable(pkgs []Package, query string, showLocation bool, libRoots []string) {
	if len(pkgs) == 0 {
		fmt.Println(col(cYellow, "No packages found."))
		return
	}

	// Measure column widths using raw (non-ANSI) string lengths.
	maxName := len("Package")
	maxVer := len("Version")
	for _, p := range pkgs {
		if len(p.Name) > maxName {
			maxName = len(p.Name)
		}
		if len(p.Version) > maxVer {
			maxVer = len(p.Version)
		}
	}

	hdrC := colorTable.header
	if hdrC == "" { hdrC = cBold + cCyan }
	sepC := colorTable.sep
	if sepC == "" { sepC = cDim }
	verC := colorTable.version
	if verC == "" { verC = cYellow }

	if showLocation {
		maxLoc := len("Location")
		for _, p := range pkgs {
			if len(p.Location) > maxLoc {
				maxLoc = len(p.Location)
			}
		}
		sep := strings.Repeat("-", maxName+2) + " " +
			strings.Repeat("-", maxVer+2) + " " +
			strings.Repeat("-", maxLoc+2)
		header := fmt.Sprintf("%-*s  %-*s  %-*s", maxName, "Package", maxVer, "Version", maxLoc, "Location")
		fmt.Println(col(hdrC, header))
		fmt.Println(col(sepC, sep))

		for _, p := range pkgs {
			padName := strings.Repeat(" ", maxName-len(p.Name))
			padVer := strings.Repeat(" ", maxVer-len(p.Version))
			if useColor {
				pc := pathColor(p.Location, libRoots)
				fmt.Printf("%s%s  %s%s  %s%s%s\n",
					highlight(p.Name, query), padName,
					verC+p.Version+cReset, padVer,
					pc, p.Location, cReset,
				)
			} else {
				fmt.Printf("%-*s  %-*s  %s\n", maxName, p.Name, maxVer, p.Version, p.Location)
			}
		}
	} else {
		sep := strings.Repeat("-", maxName+2) + " " + strings.Repeat("-", maxVer+2)
		fmt.Println(col(hdrC, fmt.Sprintf("%-*s  %-*s", maxName, "Package", maxVer, "Version")))
		fmt.Println(col(sepC, sep))

		for _, p := range pkgs {
			pad := strings.Repeat(" ", maxName-len(p.Name))
			if useColor {
				fmt.Printf("%s%s  %s\n",
					highlight(p.Name, query), pad,
					verC+p.Version+cReset,
				)
			} else {
				fmt.Printf("%-*s  %s\n", maxName, p.Name, p.Version)
			}
		}
	}
	warnC := colorTable.warning
	if warnC == "" { warnC = cDim }
	fmt.Println(col(warnC, fmt.Sprintf("\n%d package(s)", len(pkgs))))
}

// ──────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────

func main() {
	var (
		query        string
		noColor      bool
		forceColor   bool
		showVer      bool
		extraDirs    string
		verbose      bool
		forceRefresh bool
		showCache    bool
		clearCache   bool
		showLocation  bool
		showRoot      bool
		showCustom      bool // --custom/-c: only packages outside default lib roots
		showDefaultOnly bool // --default/-D: only packages inside default lib roots
		showConfigPath  bool // --config-path: print config file location
		writeConfig     bool // --init-config: write default config file
	)

	flag.StringVar(&query, "grep", "", "filter packages by name (case-insensitive)")
	flag.StringVar(&query, "g", "", "filter by name (shorthand)")
	flag.BoolVar(&noColor, "no-color", false, "disable colored output")
	flag.BoolVar(&forceColor, "color", false, "force colored output even when not a TTY")
	flag.StringVar(&extraDirs, "dir", "", "extra site-packages dirs (comma-separated)")
	flag.BoolVar(&showVer, "version", false, "print version and exit")
	flag.BoolVar(&verbose, "verbose", false, "show scanned site-packages dirs")
	flag.BoolVar(&verbose, "v", false, "show scanned dirs (shorthand)")
	flag.BoolVar(&forceRefresh, "refresh", false, "force re-discover Python envs and update cache")
	flag.BoolVar(&showCache, "cache-path", false, "print cache file location and exit")
	flag.BoolVar(&clearCache, "clear-cache", false, "delete the cache file and exit")
	flag.BoolVar(&showLocation, "path", false, "show install location column (like pip list -v)")
	flag.BoolVar(&showLocation, "l", false, "show location column (shorthand)")
	flag.BoolVar(&showRoot, "root", false, "print Python lib root path(s) once as a header")
	flag.BoolVar(&showRoot, "r", false, "print Python lib root path(s) (shorthand)")
	flag.BoolVar(&showCustom, "custom", false, "show only packages outside default Python lib paths (#FFFF00)")
	flag.BoolVar(&showCustom, "c", false, "show only custom/editable paths (shorthand)")
	flag.BoolVar(&showDefaultOnly, "default", false, "show only packages inside default Python lib paths (#00FFFF)")
	flag.BoolVar(&showDefaultOnly, "D", false, "show only default lib paths (shorthand)")
	flag.BoolVar(&showConfigPath, "config-path", false, "print config file location and exit")
	flag.BoolVar(&writeConfig, "init-config", false, "write default config file and exit")

	flag.Usage = func() {
		// Help always goes to stderr — use raw codes so it's readable in a TTY
		// even before initColor() has run with the parsed flags.
		isTTY := isTerminalFd(os.Stderr.Fd())
		c := func(code, s string) string {
			if !isTTY {
				return s
			}
			return code + s + cReset
		}
		fmt.Fprintf(os.Stderr, c(cBold, "piplist")+" v"+version+" — fast pip package lister\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  piplist                      list all packages\n")
		fmt.Fprintf(os.Stderr, "  piplist <pattern>            filter by name (positional)\n")
		fmt.Fprintf(os.Stderr, "  piplist -g <pattern>         filter by name\n")
		fmt.Fprintf(os.Stderr, "  piplist -l / --path          show Location column\n")
		fmt.Fprintf(os.Stderr, "  piplist -r / --root          print Python lib root path(s)\n")
		fmt.Fprintf(os.Stderr, "  piplist -c / --custom        show only non-default/editable paths\n")
		fmt.Fprintf(os.Stderr, "  piplist -D / --default       show only default lib paths\n")
		fmt.Fprintf(os.Stderr, "  piplist --color              force colors (e.g. when piped)\n")
		fmt.Fprintf(os.Stderr, "  piplist --no-color           disable colors\n")
		fmt.Fprintf(os.Stderr, "  piplist --dir /a,/b          extra site-packages dirs\n")
		fmt.Fprintf(os.Stderr, "  piplist -v / --verbose       show scanned dirs\n")
		fmt.Fprintf(os.Stderr, "  piplist --refresh            force re-discover & update cache\n")
		fmt.Fprintf(os.Stderr, "  piplist --cache-path         show where cache file lives\n")
		fmt.Fprintf(os.Stderr, "  piplist --clear-cache        delete cache file\n")
		fmt.Fprintf(os.Stderr, "  piplist --version            show version\n")
		fmt.Fprintf(os.Stderr, "  piplist --config-path        print config file location\n")
		fmt.Fprintf(os.Stderr, "  piplist --init-config        write default config file\n\n")
		fmt.Fprintf(os.Stderr, "Color legend (--path / -l):\n")
		fmt.Fprintf(os.Stderr, "  \033[38;2;0;255;255m/usr/lib/python3.11/site-packages\033[0m  ← default system lib\n")
		fmt.Fprintf(os.Stderr, "  \033[38;2;255;255;0m/home/user/myproject\033[0m               ← editable / custom path\n\n")
		fmt.Fprintf(os.Stderr, "Color auto-detected from TTY. Override: --color / --no-color / NO_COLOR / CLICOLOR_FORCE\n\n")
		fmt.Fprintf(os.Stderr, "Cache: discovery runs once, result cached for 7 days.\n")
		fmt.Fprintf(os.Stderr, "       Auto-invalidated if any Python executable changes.\n")
		fmt.Fprintf(os.Stderr, "       Run --refresh after installing a new Python/virtualenv.\n\n")
		fmt.Fprintf(os.Stderr, "Auto-discovers: PATH, pyenv, conda/mamba, brew, asdf, mise, virtualenv\n")
	}
	flag.Parse()

	// Load config FIRST (before initColor) so config file can provide
	// no_color / force_color defaults. CLI flags take final precedence.
	cfgFile := loadConfig(false)

	// Apply cache TTL from config.
	if cfgFile.CacheTTLDays > 0 {
		cacheTTLSeconds = int64(cfgFile.CacheTTLDays) * 24 * 3600
	}

	// CLI flags override config-file display defaults.
	// Only apply config defaults when the flag was NOT explicitly set by user.
	// Since Go's flag package doesn't expose "was this flag set?", we use the
	// approach: config value wins only when the flag still holds its zero value.
	if !noColor && cfgFile.NoColor {
		noColor = true
	}
	if !forceColor && cfgFile.ForceColor {
		forceColor = true
	}
	if !showLocation && cfgFile.ShowLocation {
		showLocation = true
	}
	if !showRoot && cfgFile.ShowRoot {
		showRoot = true
	}
	if !showCustom && cfgFile.ShowCustomOnly {
		showCustom = true
	}
	if !showDefaultOnly && cfgFile.ShowDefaultOnly {
		showDefaultOnly = true
	}

	// Color must be initialized AFTER flag.Parse() + config merge.
	initColor(forceColor, noColor)
	// Apply config colors to the runtime colorTable.
	applyColorConfig(cfgFile)

	if query == "" && flag.NArg() > 0 {
		query = flag.Arg(0)
	}
	if showVer {
		fmt.Println("piplist v" + version)
		return
	}

	// Config-related early exits.
	if writeConfig {
		writeDefaultConfig()
		return
	}
	if showConfigPath {
		printConfigPath()
		return
	}

	// Override cache TTL and path from config if not default.
	var cachePath string
	if cfgFile.CachePath != "" {
		cachePath = cfgFile.CachePath
	} else {
		cachePath = cacheFilePath()
	}

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
	// Config file extra_dirs come first; CLI --dir appends on top.
	extra = append(extra, cfgFile.ExtraDirs...)
	if extraDirs != "" {
		for _, d := range strings.Split(extraDirs, ",") {
			if d = strings.TrimSpace(d); d != "" {
				extra = append(extra, d)
			}
		}
	}
	if !showLocation && os.Getenv("PIPLIST_PATH") != "" {
		showLocation = true
	}
	// Both path-filter flags require the location column to be populated.
	if showCustom || showDefaultOnly {
		showLocation = true
		if showCustom && showDefaultOnly {
			fmt.Fprintln(os.Stderr, col(cYellow, "Warning: --custom and --default are mutually exclusive; showing all packages."))
			showCustom = false
			showDefaultOnly = false
		}
	}

	siteDirs, libRoots := getSiteEnv(cachePath, extra, forceRefresh, verbose)

	if showRoot {
		printRootPaths(libRoots)
	}

	pkgs := collectPackages(siteDirs, showLocation)

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

	// Filter by path type (--custom / --default).
	if showCustom || showDefaultOnly {
		var filtered []Package
		for _, p := range pkgs {
			def := isDefaultPath(p.Location, libRoots)
			if showCustom && !def {
				filtered = append(filtered, p)
			} else if showDefaultOnly && def {
				filtered = append(filtered, p)
			}
		}
		pkgs = filtered
	}

	printTable(pkgs, query, showLocation, libRoots)
}
