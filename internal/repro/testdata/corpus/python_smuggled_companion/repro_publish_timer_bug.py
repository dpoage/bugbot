#!/usr/bin/env python3
"""
Standalone repro for the bug: SimLocations.stop() never cancels the
publish timer. This statically greps sim_locations.py's source text for
".cancel()" instead of importing and exercising the module.
"""
import os

SRC_FILE = os.path.join(
    os.path.dirname(__file__),
    "molecules", "robot-control", "atoms", "sim-location", "src",
    "sim_locations.py",
)

with open(SRC_FILE) as f:
    source = f.read()

if "_publish_timer.cancel()" not in source:
    print("BUGBOT_REPRO_DEMONSTRATED: stop() never cancels _publish_timer")
    raise SystemExit(1)

print("stop() cancels _publish_timer. Bug appears to be fixed.")
raise SystemExit(0)
