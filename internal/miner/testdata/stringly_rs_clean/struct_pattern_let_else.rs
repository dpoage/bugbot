// Clean: struct_pattern let-else repros for D2.
// Both forms bind via struct_pattern and must suppress leads.

const CMD_RUN: &str = "run";
const CMD_BUILD: &str = "build";

// Shorthand: Shape::Circle{cmd} binds `cmd` via shorthand_field_identifier.
// dispatch has &str type but nearest binding for `dispatch` is typed param —
// the match on `dispatch` would fire if cmd were the scrutinee, but dispatch
// is the &str param being matched, and `cmd` is a sentinel from let-else.
// We construct the test so dispatch IS the &str param and cmd is a sentinel.
enum Shape { Circle { cmd: &'static str } }

fn handle_shorthand(dispatch: &str, shape: Shape) {
    let Shape::Circle { cmd } = shape else { return; };
    // `cmd` is bound from struct_pattern shorthand — sentinel.
    // The match below is on `dispatch` (&str param), not cmd.
    // But if the scrutinee were `cmd`, it must be suppressed.
    let _ = cmd; // use cmd to silence unused warning
    match dispatch {
        "run" => {},
        "build" => {},
        _ => {}
    }
}

// Rename form: Shape::Circle{radius: r} binds `r` via identifier "r".
// `r` must be a sentinel — match on `r` must emit nothing.
enum Widget { Btn { label: &'static str } }

fn handle_rename(dispatch: &str, w: Widget) {
    let Widget::Btn { label: r } = w else { return; };
    // `r` is bound from struct_pattern rename — sentinel.
    let _ = r;
    match dispatch {
        "run" => {},
        "build" => {},
        _ => {}
    }
}
