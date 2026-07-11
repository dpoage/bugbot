/* Clean: switch has default — type-B suppressed even though STATUS_DONE
   is not covered. The default is an explicit subset idiom, invisible to
   -Wswitch; our miner respects this choice. */

void check(Status s) {
    switch (s) {
        case STATUS_OK:
            break;
        case STATUS_ERROR:
            break;
        default:
            break;
    }
}
