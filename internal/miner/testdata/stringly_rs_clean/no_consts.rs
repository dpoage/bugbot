// Clean: no const &str producers in the file; miner must emit nothing.

fn dispatch(status: &str) {
    match status {
        "active" => println!("go"),
        "inactive" => println!("stop"),
        _ => {}
    }
}
