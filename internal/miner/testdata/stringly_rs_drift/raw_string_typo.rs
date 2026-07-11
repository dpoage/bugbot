// Positive fixture: raw string arm has a typo not in the producer set.

const PATH_FOO: &str = r"hello";
const PATH_BAR: &str = r"world";

fn dispatch(s: &str) {
    match s {
        r"hello" => {},
        r"wrold" => {},  // typo: "wrold" vs "world" — LINE 9
        _ => {}
    }
}
