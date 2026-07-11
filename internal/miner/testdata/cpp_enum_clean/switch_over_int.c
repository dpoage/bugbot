/* Clean: switch scrutinee is a plain int parameter — no type binding
   to a known enum, so no lead is emitted even though the literal values
   happen to match Status enumerator values. */

void process_code(int code) {
    switch (code) {
        case 0:  /* same value as STATUS_OK but type is int, not Status */
            break;
        case 1:
            break;
    }
}
