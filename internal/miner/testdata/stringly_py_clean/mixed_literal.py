# Negative: Literal with mixed str|int types — structural whitelist excludes it.
# Expected: 0 leads (mixed Literal not treated as producer).

Mixed = Literal['active', 1, 'inactive']


def handle(status: Mixed) -> None:
    if status == 'active':
        pass
    elif status == 'stale':   # would be type-A but Mixed is excluded
        pass
