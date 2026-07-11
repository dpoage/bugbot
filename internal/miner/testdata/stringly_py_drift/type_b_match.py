# Positive fixture: Type-B defect in match statement.
# Expected: lead at the match statement line (missing 'green').

from enum import StrEnum


class Color(StrEnum):
    RED = 'red'
    GREEN = 'green'
    BLUE = 'blue'


def paint(color: Color) -> None:
    match color:
        case 'red':
            pass
        case 'blue':
            pass
        # 'green' never handled and no case _:
