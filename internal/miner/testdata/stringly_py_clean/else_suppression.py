# Negative: else clause suppresses Type-B (explicit-subset is valid).
# Expected: 0 leads for type-B (else present).

Status = Literal['active', 'inactive', 'pending']


def handle(status: Status) -> None:
    if status == 'active':
        pass
    elif status == 'inactive':
        pass
    else:
        # handles 'pending' and any future members
        pass
