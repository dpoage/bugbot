# Negative: match-case CAPTURE pattern shadows outer typed param.
# `case status:` binds the subject to `status` inside the case body.
# That capture binding is nearer than the outer typed param → no join.
# Expected: 0 leads.

Status = Literal['active', 'inactive']


def handle(status: Status) -> None:
    match status:
        case status:          # capture pattern: binds status to match subject
            if status == 'stale':   # would be type-A but capture shadow applies
                pass
