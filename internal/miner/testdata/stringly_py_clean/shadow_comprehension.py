# Negative: comprehension variable shadows outer typed param name.
# Expected: 0 leads (comprehension for-in binding is nearer sentinel).

Status = Literal['active', 'inactive']


def process(status: Status) -> None:
    # The comprehension creates a new scope with its own 'status' binding.
    result = [status for status in ['stale', 'unknown']]
    # The if below uses a fresh local; the comprehension scope is closest binding.
    if status == 'stale':   # would be type-A but comprehension shadow applies
        pass
