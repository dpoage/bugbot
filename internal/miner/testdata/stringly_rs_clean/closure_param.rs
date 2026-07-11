// Clean: closure parameter shadows the outer &str binding — emit nothing.
// The nearest binding for `state` inside the closure is the closure param
// (untyped sentinel), not the function parameter.

const S_ACTIVE: &str = "active";
const S_IDLE: &str = "idle";

fn process(items: Vec<&str>, state: &str) {
    // `state` param is &str-typed. Inside the closure, a new `state` binding
    // (closure param) is introduced — it shadows the outer typed param.
    items.iter().for_each(|state| {
        match state {
            "active" => {},
            "xdle" => {},  // would be type-A but nearest binding is closure param
            _ => {}
        }
    });
    // Outside the closure, `state` still resolves to the typed param.
    // But there are no string-arm mismatches here.
    match state {
        "active" => {},
        "idle" => {},
        _ => {}
    }
}
