# Negative: match with case _: suppresses Type-B.
# Expected: 0 leads (wildcard present).

Status = Literal['active', 'inactive', 'pending']


def handle(status: Status) -> None:
    match status:
        case 'active':
            pass
        case 'inactive':
            pass
        case _:
            # handles 'pending' and any future members
            pass
