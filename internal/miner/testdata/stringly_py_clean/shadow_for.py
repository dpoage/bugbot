# Negative: for-loop variable shadows outer typed param.
# Expected: 0 leads (for binding is nearer than typed param).

Status = Literal['active', 'inactive']


def process(status: Status) -> None:
    for status in ['any', 'value']:   # for binding shadows typed param
        if status == 'stale':         # stale not in Status but shadow prevents lead
            pass
