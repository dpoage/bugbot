// Clean: raw string literals in match arms all present in producer set.

const PATH_A: &str = r"hello";
const PATH_B: &str = r"world";

fn dispatch(s: &str) {
    match s {
        r"hello" => {},
        r"world" => {},
        _ => {}
    }
}
