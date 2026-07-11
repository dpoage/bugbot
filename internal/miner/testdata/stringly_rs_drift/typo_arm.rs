// Positive fixture: &str match arm with a typo not in the const producer set.
// Expected lead: line 9 ("inactve" is not in {active, inactive, pending}).

const STATUS_ACTIVE: &str = "active";
const STATUS_INACTIVE: &str = "inactive";
const STATUS_PENDING: &str = "pending";

fn dispatch(status: &str) {
    match status {
        "active" => println!("go"),
        "inactve" => println!("stop"),  // typo: missing 'i' — LINE 11
        "pending" => println!("wait"),
        _ => {}
    }
}
