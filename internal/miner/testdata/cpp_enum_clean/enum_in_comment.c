/* Clean: enum name STATUS_OK appears in a comment and string — not in
   an actual case arm. Tree-sitter skips comment/string content. */

void do_thing(Status s) {
    /* STATUS_DONE could be handled here but is not needed */
    switch (s) {
        case STATUS_OK:
            break;
        case STATUS_ERROR:
            break;
        default:
            break; /* handles STATUS_DONE via default */
    }
}
