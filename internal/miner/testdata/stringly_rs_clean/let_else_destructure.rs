// Clean: D2 oracle repro — let-else destructure `let Some(cmd) = opt else { return; }`
// binds `cmd` via tuple_struct_pattern. Without the D2 fix, `cmd` was invisible
// to the sentinel pass, and the outer &str param `cmd` (if present) would leak
// through, producing a false lead. With the fix, `cmd` is registered as an
// untyped sentinel in the enclosing block's scope, suppressing the lead.

const CMD_RUN: &str = "run";
const CMD_BUILD: &str = "build";

fn dispatch(opt: Option<&str>) {
    let Some(cmd) = opt else { return; };
    // `cmd` is now bound via let-else destructure (tuple_struct_pattern).
    // Its type is &str at runtime but unknown to the miner — must be sentinel.
    match cmd {
        "run" => {},
        "bild" => {},   // would be type-A if cmd were typed; must emit nothing
        _ => {}
    }
}
