"""Run with: go run ./cmd/gopy examples/script.py Alice"""

import platform
import sys

name = sys.argv[1] if len(sys.argv) > 1 else "world"
print(f"Hello, {name}!")
print(f"Python {platform.python_version()} running on {sys.platform}")
