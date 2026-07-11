# Positive fixture: Type-A defect — branch literal not in Literal set.
# Expected: lead at line 11 ('stale' is not a member of Status).

Status = Literal['active', 'inactive', 'pending']


def handle(status: Status) -> None:
    if status == 'active':
        pass
    elif status == 'stale':   # BUG: 'stale' not in Status
        pass
    elif status == 'inactive':
        pass
