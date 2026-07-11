# Positive fixture: Type-B defect — member 'pending' never handled.
# Expected: lead at line 7 (if_statement line, missing 'pending').

Status = Literal['active', 'inactive', 'pending']


def handle(status: Status) -> None:
    if status == 'active':
        pass
    elif status == 'inactive':
        pass
    # 'pending' never handled and no else clause
