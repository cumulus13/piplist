# piplist

A blazing-fast replacement for `pip list | grep <something>`, written in Go.  
Reads pip package metadata **directly from disk** — no Python, no subprocess, no startup overhead.

## Benchmark

| Command              | Time   |
|----------------------|--------|
| `piplist`            | ~12ms  |
| `pip list`           | ~684ms |

**~57x faster** than pip.

## Usage

```bash
# List all packages
piplist

# Filter by name (positional or flag — both work)
piplist requests
piplist -g req
piplist --grep django

# Pipe-friendly (no ANSI colors)
piplist --no-color | less
piplist --no-color > packages.txt

# Add extra site-packages dirs to scan
piplist --dir /path/to/venv/lib/python3.11/site-packages

# Show version
piplist --version
```

## Install

### Option 1 — Build & install with the script (recommended)

```bash
chmod +x install.sh
./install.sh
```

### Option 2 — Manual

```bash
go build -ldflags="-s -w" -o piplist .
sudo mv piplist /usr/local/bin/
```

### Option 3 — Install to ~/.local/bin (no sudo)

```bash
go build -ldflags="-s -w" -o piplist .
mkdir -p ~/.local/bin
mv piplist ~/.local/bin/
# Make sure ~/.local/bin is in your PATH:
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc && source ~/.bashrc
```

## How it works

`piplist` scans these directories for `*.dist-info/METADATA` and `*.egg-info/PKG-INFO` files:

- `/usr/lib/pythonX.Y/site-packages`
- `/usr/lib/pythonX.Y/dist-packages`
- `/usr/local/lib/pythonX.Y/site-packages`
- `~/.local/lib/pythonX.Y/site-packages`
- `$VIRTUAL_ENV/lib/pythonX.Y/site-packages`
- (auto-detected for all platforms)

No Python interpreter is invoked at any point.

## Requirements

- Go 1.18+ (only needed to build; the compiled binary has **zero runtime dependencies**)

## 👤 Author
        
[Hadi Cahyadi](mailto:cumulus13@gmail.com)
    

[![Buy Me a Coffee](https://www.buymeacoffee.com/assets/img/custom_images/orange_img.png)](https://www.buymeacoffee.com/cumulus13)

[![Donate via Ko-fi](https://ko-fi.com/img/githubbutton_sm.svg)](https://ko-fi.com/cumulus13)
 
[Support me on Patreon](https://www.patreon.com/cumulus13)