// Clean: D1 oracle repro — const COLOR_RED="#ff0000" is semantically unrelated
// to the HTTP verb match. Zero arm literals overlap the producer pool, so the
// anchor check fires and the miner emits nothing.

const COLOR_RED: &str = "#ff0000";
const COLOR_BLUE: &str = "#0000ff";
const COLOR_GREEN: &str = "#00ff00";

fn handle_method(method: &str) {
    match method {
        "GET" => {},
        "POST" => {},
        "PUT" => {},
        _ => {}
    }
}
