#!/usr/bin/env python
"""Run the whole suite. One command, no arguments, no packages to install:

    python tests/run.py

Works from any directory, which is why it exists rather than a bare unittest discover --
the tests import each other and the engine, and discover only finds them from the right
working directory. Exits non-zero if anything fails, so CI and hooks can use it.
"""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)  # so the test modules can import helpers
sys.path.insert(0, os.path.dirname(HERE))  # so helpers can import kalshi_trader


def main():
    suite = unittest.defaultTestLoader.discover(HERE, top_level_dir=HERE)
    verbosity = 2 if "-v" in sys.argv else 1
    result = unittest.TextTestRunner(verbosity=verbosity).run(suite)
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    sys.exit(main())
