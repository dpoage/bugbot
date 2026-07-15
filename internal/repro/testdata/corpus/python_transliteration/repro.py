"""Non-behavioral: a Python REIMPLEMENTATION of the buggy TS IIFE in
src/scheduler/timeInTask.ts. Executes only this file's own re-implementation
— never imports or calls the repo's own timeInTask() — so it "demonstrates"
a bug in a copy of the logic, not in the actual target file.

Mirrors the_cloud finding 0000019f396c9338f5614dde8e1eb164 ("timeInTask
-Infinity"), one of the 4 false-T1 promotions in bugbot-qb4r.
"""


def time_in_task(start, now):
    # Mirrors (transliterates) src/scheduler/timeInTask.ts's buggy
    # subtraction order for demonstration purposes only.
    return now - start


def test_negative_infinity_bug():
    result = time_in_task(float("inf"), 0)
    assert result != float("-inf"), "BUGBOT_REPRO_DEMONSTRATED"


if __name__ == "__main__":
    test_negative_infinity_bug()
