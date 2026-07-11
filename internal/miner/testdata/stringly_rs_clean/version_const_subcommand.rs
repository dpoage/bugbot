// Clean: D1 oracle repro — const VERSION="1.4.2" is semantically unrelated
// to the subcommand dispatch match. Zero arm literals overlap the producer
// pool, so the anchor check fires and the miner emits nothing.

const VERSION: &str = "1.4.2";

fn dispatch(cmd: &str) {
    match cmd {
        "run" => {},
        "build" => {},
        "test" => {},
        _ => {}
    }
}
