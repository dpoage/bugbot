// Clean: match expression inside macro_rules! body is not a real match_expression
// AST node — the grammar parses it as token_tree. Miner must emit nothing.

const FOO: &str = "foo";
const BAR: &str = "bar";

macro_rules! dispatch_event {
    ($x:expr) => {
        match $x {
            "foo" => 1,
            "baz" => 2,  // "baz" not in producers, but inside macro_rules — must NOT emit
            _ => 0,
        }
    }
}

fn use_macro(s: &str) {
    let _ = dispatch_event!(s);
}
