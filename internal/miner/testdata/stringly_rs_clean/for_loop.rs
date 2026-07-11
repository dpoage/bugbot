// Clean: for-loop variable `item` is an untyped sentinel binding.
// Even though there are const producers and `item` ends up being &str at
// runtime, the miner cannot prove this statically — emit nothing.

const TAG_A: &str = "a";
const TAG_B: &str = "b";

fn scan(tags: &[&str]) {
    for item in tags {
        match item {
            "a" => {},
            "c" => {},  // "c" not in producers, but `item` is a for-loop var — suppress
            _ => {}
        }
    }
}
