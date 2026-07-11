# Negative: lambda parameter shadows outer typed param.
# The lambda 'status' binding is nearer than the typed outer param.
# Expected: 0 leads (lambda shadow prevents join).

Status = Literal['active', 'inactive']


def outer(status: Status) -> None:
    check = lambda status: status == 'stale'  # lambda shadows typed param
    if check('stale'):
        pass
