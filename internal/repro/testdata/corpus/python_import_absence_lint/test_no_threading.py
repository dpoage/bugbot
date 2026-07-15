"""Non-behavioral: a lint check on main.py's SOURCE TEXT — asserts it does
not mention a threading primitive. Never imports or runs main.py, so no
race is ever actually executed.

Mirrors the_cloud finding 0000019f61d145d51d39a947952354fc ("Data race on
_sleep_time"), one of the 4 false-T1 promotions in bugbot-qb4r.
"""


def test_main_does_not_use_threading():
    with open("agent/main.py") as f:
        src = f.read()
    assert "threading.Lock" not in src
