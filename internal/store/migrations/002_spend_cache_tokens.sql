-- Prompt-cache accounting on the spend ledger.
--
-- input_tokens remains the TOTAL prompt size (including cached tokens, per the
-- llm.Usage normalization convention), so existing budget math is unchanged.
-- cache_read_tokens / cache_creation_tokens are informational subsets of
-- input_tokens used to report cache savings (reads bill at ~0.1x on Anthropic,
-- 0.25-0.5x on OpenAI; creation bills at 1.25x on Anthropic).

ALTER TABLE spend ADD COLUMN cache_read_tokens     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE spend ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;
