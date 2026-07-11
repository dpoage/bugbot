# Negative: Enum class with non-string values — structural whitelist excludes it.
# Expected: 0 leads (non-string member present).

from enum import Enum


class Mixed(Enum):
    OK = 'ok'
    ERROR = 'error'
    CODE = 42     # non-string value → excluded from producers


def handle(status: Mixed) -> None:
    if status == 'ok':
        pass
    elif status == 'stale':   # would be type-A but Mixed excluded
        pass
