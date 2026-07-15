import unittest


class TestSourceContainsFix(unittest.TestCase):
    """Non-behavioral: reads SelectedContractStore.ts as TEXT and greps for a
    substring. Never imports or executes the target file's logic.

    Mirrors the_cloud finding 0000019f396131b192d73b758917369f ("Stale
    selectedContract"), one of the 4 false-T1 promotions in bugbot-qb4r.
    """

    def test_uses_get_value(self):
        with open("src/store/SelectedContractStore.ts") as f:
            src = f.read()
        self.assertIn("SelectedContractStore.getValue()", src)


if __name__ == "__main__":
    unittest.main()
