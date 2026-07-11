# Positive fixture: Type-A defect in match statement.
# Expected: lead at line 12 ('typo_case' not in Color).

from enum import StrEnum


class Color(StrEnum):
    RED = 'red'
    GREEN = 'green'
    BLUE = 'blue'


def paint(color: Color) -> None:
    match color:
        case 'red':
            pass
        case 'typo_case':    # BUG: not a member of Color
            pass
        case 'blue':
            pass
