// File: config.go
// Author: Hadi Cahyadi <cumulus13@gmail.com>
// Description: Config file support for piplist using go-config-get.
//              Config file: piplist.ini (or .toml/.json/.yaml/.env)
//              Location: platform-standard dirs (XDG on Linux/macOS, %APPDATA% on Windows)
// License: MIT

package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/cumulus13/go-config-get/configget"
)

// ──────────────────────────────────────────────
// PiplistConfig holds every tunable setting.
// Values here are the live runtime settings after merging:
//   config file → env vars (via go-config-get) → CLI flags (highest priority)
// ──────────────────────────────────────────────

type PiplistConfig struct {
	// [colors]
	ColorName        string // package name column            default: #00FF00 (green)
	ColorVersion     string // version column                 default: #FFFF00 (yellow)
	ColorHeader      string // table header                   default: bold+cyan
	ColorSep         string // separator line                 default: dim
	ColorPathDefault string // location inside lib root       default: #00FFFF
	ColorPathCustom  string // location outside lib root      default: #FFFF00
	ColorHighlight   string // matched search text            default: bold+red
	ColorWarning     string // warning messages               default: yellow
	ColorRootHeader  string // --root header label            default: bold+cyan

	// [display]
	ShowLocation    bool // -l / --path
	ShowRoot        bool // -r / --root
	ShowCustomOnly  bool // -c / --custom
	ShowDefaultOnly bool // -D / --default
	NoColor         bool // --no-color
	ForceColor      bool // --color

	// [cache]
	CacheTTLDays int    // days before cache expires   default: 7
	CachePath    string // override cache file path    default: platform default

	// [search]
	ExtraDirs []string // additional site-packages dirs to scan
}

// defaultConfig returns hardcoded defaults — these are what you get with no
// config file present.
func defaultConfig() PiplistConfig {
	return PiplistConfig{
		ColorName:        "#00FF00",
		ColorVersion:     "#FFFF00",
		ColorHeader:      "bold+cyan",
		ColorSep:         "dim",
		ColorPathDefault: "#00FFFF",
		ColorPathCustom:  "#FFFF00",
		ColorHighlight:   "bold+red",
		ColorWarning:     "yellow",
		ColorRootHeader:  "bold+cyan",
		CacheTTLDays:     7,
	}
}

// ──────────────────────────────────────────────
// loadConfig discovers and reads the config file, then returns a
// PiplistConfig with all values resolved (file < env vars < CLI flags).
// Missing keys fall back to defaultConfig(). Never fatal — on any error
// the function returns defaults so piplist always works.
// ──────────────────────────────────────────────

func loadConfig(showConfigPath bool) PiplistConfig {
	cfg := defaultConfig()

	// On Windows, APPDATA may be corrupted into a PATH-like string
	// (e.g. "C:\Program Files\PowerShell\7;C:\Users\...\AppData\Roaming").
	// go-config-get reads APPDATA internally, so sanitize it first.
	if runtime.GOOS == "windows" {
		if strings.Contains(os.Getenv("APPDATA"), ";") {
			os.Setenv("APPDATA", winUserDataDir())
		}
	}

	// Restrict to .ini only.
	// The .env format has NO section support (flat KEY=VALUE only), which means
	// WithSection() calls would silently return defaults for every key.
	// .ini is the right format for this config: lightweight, human-readable, sections.
	cg := configget.New("piplist.ini", "piplist", configget.Options{
		Create:     true,
		Extensions: []string{".ini"},
	})

	if showConfigPath {
		if p, err := cg.Path(); err == nil {
			fmt.Fprintln(os.Stderr, "Config file:", p)
		} else {
			fmt.Fprintln(os.Stderr, "Config file: (not found —", err, ")")
		}
	}

	colors := cg.WithSection("colors")
	display := cg.WithSection("display")
	cache := cg.WithSection("cache")
	search := cg.WithSection("search")

	// ── [colors] ──────────────────────────────────────────────────────────
	cfg.ColorName = colors.String("color_name", cfg.ColorName)
	cfg.ColorVersion = colors.String("color_version", cfg.ColorVersion)
	cfg.ColorHeader = colors.String("color_header", cfg.ColorHeader)
	cfg.ColorSep = colors.String("color_sep", cfg.ColorSep)
	cfg.ColorPathDefault = colors.String("color_path_default", cfg.ColorPathDefault)
	cfg.ColorPathCustom = colors.String("color_path_custom", cfg.ColorPathCustom)
	cfg.ColorHighlight = colors.String("color_highlight", cfg.ColorHighlight)
	cfg.ColorWarning = colors.String("color_warning", cfg.ColorWarning)
	cfg.ColorRootHeader = colors.String("color_root_header", cfg.ColorRootHeader)

	// ── [display] ─────────────────────────────────────────────────────────
	cfg.ShowLocation = display.Bool("show_location", cfg.ShowLocation)
	cfg.ShowRoot = display.Bool("show_root", cfg.ShowRoot)
	cfg.ShowCustomOnly = display.Bool("show_custom_only", cfg.ShowCustomOnly)
	cfg.ShowDefaultOnly = display.Bool("show_default_only", cfg.ShowDefaultOnly)
	cfg.NoColor = display.Bool("no_color", cfg.NoColor)
	cfg.ForceColor = display.Bool("force_color", cfg.ForceColor)

	// ── [cache] ───────────────────────────────────────────────────────────
	cfg.CacheTTLDays = int(cache.Int("ttl_days", int64(cfg.CacheTTLDays)))
	if cfg.CacheTTLDays < 1 {
		cfg.CacheTTLDays = 1
	}
	cfg.CachePath = cache.String("path", cfg.CachePath)

	// ── [search] ──────────────────────────────────────────────────────────
	if raw := search.String("extra_dirs", ""); raw != "" {
		for _, d := range strings.Split(raw, ",") {
			if d = strings.TrimSpace(d); d != "" {
				cfg.ExtraDirs = append(cfg.ExtraDirs, d)
			}
		}
	}

	return cfg
}

// ──────────────────────────────────────────────
// ANSI code generation from config color strings
// ──────────────────────────────────────────────

// resolveColor converts a color string from the config file into an ANSI
// escape sequence prefix (without reset). Supported formats:
//
//	#RRGGBB               → 24-bit truecolor  \033[38;2;R;G;Bm
//	#RGB                  → 24-bit truecolor  (expanded)
//	bold+cyan, dim+red    → attribute+named combinations
//	green, yellow, …      → standard 8-color names
//	bold, dim, italic     → attributes alone
//	""                    → "" (no coloring)
//
// Combinations use "+" as separator: "bold+#00FF88", "dim+blue".
func resolveColor(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "+")
	var codes []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if c := namedAnsi(p); c != "" {
			codes = append(codes, c)
			continue
		}
		// try #RRGGBB / #RGB
		if strings.HasPrefix(p, "#") {
			r, g, b, ok := parseHex(p)
			if ok {
				codes = append(codes, fmt.Sprintf("38;2;%d;%d;%d", r, g, b))
			}
			continue
		}
		// try plain hex without # (e.g. "00ff00")
		if len(p) == 6 || len(p) == 3 {
			r, g, b, ok := parseHex("#" + p)
			if ok {
				codes = append(codes, fmt.Sprintf("38;2;%d;%d;%d", r, g, b))
				continue
			}
		}
	}
	if len(codes) == 0 {
		return ""
	}
	return "\033[" + strings.Join(codes, ";") + "m"
}

func namedAnsi(s string) string {
	switch s {
	case "reset":
		return "0"
	case "bold":
		return "1"
	case "dim", "faint":
		return "2"
	case "italic":
		return "3"
	case "underline":
		return "4"
	case "black":
		return "30"
	case "red":
		return "31"
	case "green":
		return "32"
	case "yellow":
		return "33"
	case "blue":
		return "34"
	case "magenta", "purple":
		return "35"
	case "cyan":
		return "36"
	case "white":
		return "37"
	case "bright_black", "dark_gray":
		return "90"
	case "bright_red":
		return "91"
	case "bright_green":
		return "92"
	case "bright_yellow":
		return "93"
	case "bright_blue":
		return "94"
	case "bright_magenta":
		return "95"
	case "bright_cyan":
		return "96"
	case "bright_white":
		return "97"
	}
	return ""
}

// parseHex parses #RRGGBB or #RGB into r,g,b uint8 values.
func parseHex(s string) (r, g, b uint8, ok bool) {
	s = strings.TrimPrefix(s, "#")
	switch len(s) {
	case 3:
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	case 6:
		// ok
	default:
		return 0, 0, 0, false
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return uint8(n >> 16), uint8(n >> 8), uint8(n), true
}

// ──────────────────────────────────────────────
// Runtime color table — applied after config load
// ──────────────────────────────────────────────

// colorTable holds the resolved ANSI codes derived from PiplistConfig.Colors.
// It replaces the top-level const color variables once config is loaded.
var colorTable struct {
	name        string // package name
	version     string // version
	header      string // table header
	sep         string // separator
	pathDefault string // #00FFFF — default lib path
	pathCustom  string // #FFFF00 — custom/editable path
	highlight   string // matched text
	warning     string // warning messages
	rootHeader  string // --root header
}

// applyColorConfig resolves all config color strings and populates colorTable.
// Must be called after initColor() so useColor is already set.
func applyColorConfig(cfg PiplistConfig) {
	if !useColor {
		// All empty — col() and friends will just return plain strings.
		return
	}
	colorTable.name = resolveColor(cfg.ColorName)
	colorTable.version = resolveColor(cfg.ColorVersion)
	colorTable.header = resolveColor(cfg.ColorHeader)
	colorTable.sep = resolveColor(cfg.ColorSep)
	colorTable.pathDefault = resolveColor(cfg.ColorPathDefault)
	colorTable.pathCustom = resolveColor(cfg.ColorPathCustom)
	colorTable.highlight = resolveColor(cfg.ColorHighlight)
	colorTable.warning = resolveColor(cfg.ColorWarning)
	colorTable.rootHeader = resolveColor(cfg.ColorRootHeader)
}

// writeDefaultConfig writes a well-commented default config file to the
// standard platform location if one doesn't exist yet. Useful for first run.
func writeDefaultConfig() {
	cg := configget.New("piplist.ini", "piplist", configget.Options{
		Create:     true,
		Extensions: []string{".ini"},
	})
	p, err := cg.Path()
	if err != nil {
		fmt.Fprintln(os.Stderr, col(cYellow, "Warning: could not resolve config path: "+err.Error()))
		return
	}

	// Only write if it doesn't exist yet
	if _, err := os.Stat(p); err == nil {
		fmt.Println("Config file already exists:", p)
		return
	}

	content := `; piplist configuration file
; Location is auto-discovered by go-config-get (XDG on Linux/macOS, %%APPDATA%% on Windows).
; All keys are optional — missing keys fall back to built-in defaults.
; Restart piplist after editing this file (no hot-reload for a CLI tool).

; ── Colors ────────────────────────────────────────────────────────────────────
; Supported formats:
;   #RRGGBB           24-bit hex  e.g. #00FF88
;   #RGB              shorthand   e.g. #0F8
;   Named colors:     black, red, green, yellow, blue, magenta, cyan, white
;                     bright_red, bright_green, bright_yellow, …
;   Attributes:       bold, dim, italic, underline
;   Combinations:     bold+cyan,  dim+#888888,  bold+#FF5500
[colors]
color_name         = #00FF00
color_version      = #FFFF00
color_header       = bold+cyan
color_sep          = dim
color_path_default = #00FFFF
color_path_custom  = #FFFF00
color_highlight    = bold+red
color_warning      = yellow
color_root_header  = bold+cyan

; ── Display defaults ──────────────────────────────────────────────────────────
; Set any of these to true to make the behaviour permanent (still overridable
; by CLI flags).
[display]
show_location    = false
show_root        = false
show_custom_only = false
show_default_only = false
no_color         = false
force_color      = false

; ── Cache ─────────────────────────────────────────────────────────────────────
[cache]
ttl_days = 7
; path =   ; leave empty to use the platform default

; ── Search ────────────────────────────────────────────────────────────────────
[search]
; extra_dirs = /path/to/extra/site-packages,/another/path
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, col(cYellow, "Warning: could not write config: "+err.Error()))
		return
	}
	fmt.Println("Default config written to:", p)
}

// printConfigPath prints the resolved config file path to stdout.
func printConfigPath() {
	cg := configget.New("piplist.ini", "piplist", configget.Options{
		Create:     false,
		Extensions: []string{".ini"},
	})
	p, err := cg.Path()
	if err != nil {
		fmt.Println("(no config file found)")
		return
	}
	fmt.Println(p)
}
