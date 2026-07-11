// Clean: all string arms are in the producer set; the wildcard is a legit catch-all.
// Miner must emit nothing (no type-A leads).

const EVT_START: &str = "start";
const EVT_STOP: &str = "stop";
const EVT_PAUSE: &str = "pause";

fn handle(event: &str) {
    match event {
        "start" => {},
        "stop" => {},
        "pause" => {},
        _ => {}  // legit catch-all for unknown events
    }
}
