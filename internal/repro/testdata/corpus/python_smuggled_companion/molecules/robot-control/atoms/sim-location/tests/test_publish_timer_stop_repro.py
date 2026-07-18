"""
Genuine behavioral test that imports and exercises SimLocations.stop().
This file IS a legitimate executable edge to the target — but plan.cmd
never runs it; it only runs repro_publish_timer_bug.py (a grep script).
Smuggling this file into plan.files must not let the grep script pass
the target-execution gate on its behalf.
"""
from src.sim_locations import SimLocations


def test_stop_cancels_publish_timer():
    sim = SimLocations()
    sim.stop()
    assert sim._publish_timer is None or not sim._publish_timer.is_alive()
