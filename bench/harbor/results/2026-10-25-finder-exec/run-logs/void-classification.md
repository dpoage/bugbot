# ypei-r1 voided attempts: 429 classification (architect, 2026-10-26)

Source: agent_units.detail in each voided trial's agent/state.db (the scan.log body is truncated at "usa..."; the MiniMax numeric code is cut off in both places, so codes cannot be read).

| attempt | lost finder units | class "Token Plan rate limit reached" (rate_limit_error) | class "Token Plan usage limit reached" (5h quota, 2056 per bugbot-hqgy loop1) | other | units that succeeded after the last failure in the same trial |
|---|---|---|---|---|---|
| exec2-a1 (2026-10-25 11:46) | 11 (xarray 8, pylint 3) | 11 | 0 | 0 | 4 |
| exec2-a2 (16:46) | 18 (astropy 2, django 7, xarray 1, pylint 3, sklearn 5) | 18 | 0 | 0 | 78 |
| exec3-a1 (21:46) | 9 (xarray 5, pylint 4) | 9 | 0 | 0 | 20 |

Failures come in bursts of 10 s to 14 min, and later units in the same trial succeed. 5h quota exhaustion does not behave that way: in loop1 it failed every later unit. The loop1 void logs (jobs/void-loop1-control) carry the other message, "Token Plan usage limit reached". Classification: all 38 lost units are a transient plan rate limit (burst/short-window), not 5h quota exhaustion.

Script: python over sqlite agent_units (finished_at ordering), /tmp/ypei-r1/harbor (see the architect transcript); query: `select role,status,started_at,finished_at,detail from agent_units order by finished_at`.

| exec2-a3 (2026-10-26 09:37) | 5 (django 1, xarray 2, pylint 2) | 5 | 0 | 0 | 91 |

exec2-a3 bursts: 15:42:40 (1 unit) and 16:08:45-16:08:50 UTC (4 units); 32/38/21 units succeeded afterwards in the same trials.
