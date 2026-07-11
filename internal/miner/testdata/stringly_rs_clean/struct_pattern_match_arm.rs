// Clean: struct_pattern match-arm repros for D2.
// Shape::Circle{cmd} => { match cmd { ... } } — cmd is bound from struct_pattern.
// Nearest binding for `cmd` inside the arm body is the struct_pattern binding
// (sentinel), not any &str-typed parameter. Must emit nothing.

const CMD_RUN: &str = "run";
const CMD_BUILD: &str = "build";

enum Shape {
    Circle { cmd: &'static str },
    Square { label: &'static str },
}

fn dispatch(shape: Shape) {
    match shape {
        // Shorthand form: {cmd} binds cmd via shorthand_field_identifier.
        Shape::Circle { cmd } => {
            match cmd {
                "run" => {},
                "bild" => {},  // typo but cmd is struct_pattern sentinel — suppress
                _ => {}
            }
        }
        // Rename form: {label: r} binds r via identifier.
        Shape::Square { label: r } => {
            match r {
                "run" => {},
                "bild" => {},  // typo but r is struct_pattern rename sentinel — suppress
                _ => {}
            }
        }
        _ => {}
    }
}
