# Positive fixture: Type-A with str+Enum mixin.
# Expected: lead at line 13 ('draft' not in State).

from enum import Enum


class State(str, Enum):
    OPEN = 'open'
    CLOSED = 'closed'
    MERGED = 'merged'


def process(state: State) -> None:
    if state == 'open':
        pass
    elif state == 'draft':    # BUG: 'draft' not in State
        pass
    elif state == 'closed':
        pass
