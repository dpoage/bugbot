// Positive fixture: match arm references a value that was removed from the
// const producer set (old value "legacy" no longer defined).
// Expected lead on the "legacy" arm line.

const ROLE_ADMIN: &str = "admin";
const ROLE_USER: &str = "user";
const ROLE_MOD: &str = "mod";

fn check_role(role: &str) -> bool {
    match role {
        "admin" => true,
        "user" => true,
        "mod" => true,
        "legacy" => true,  // stale: "legacy" was removed — LINE 14
        _ => false,
    }
}
