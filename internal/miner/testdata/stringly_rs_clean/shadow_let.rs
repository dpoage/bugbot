// Clean: shadowing re-let makes the scrutinee unresolved — emit nothing.
// The nearest binding for `s` at the match is the let_declaration (untyped),
// not the &str parameter. Miner must suppress.

const STATE_A: &str = "a";
const STATE_B: &str = "b";

fn test(s: &str) {
    let s = s.trim();  // shadow: rebinds `s` with unknown type
    match s {
        "a" => {},
        "c" => {},  // would be type-A but `s` is shadowed — must emit nothing
        _ => {}
    }
}
