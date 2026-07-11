// Positive: arms DO overlap the producer pool (anchor satisfied) AND there is
// one typo arm. The miner must emit exactly 1 lead for "pendig".
// "active" and "inactive" are in the pool — overlap=2. "pendig" is not.

const STATUS_ACTIVE: &str = "active";
const STATUS_INACTIVE: &str = "inactive";
const STATUS_PENDING: &str = "pending";

fn check(status: &str) {
    match status {
        "active" => {},
        "inactive" => {},
        "pendig" => {},   // typo: missing 'n' — LINE 13
        _ => {}
    }
}
