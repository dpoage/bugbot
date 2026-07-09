-- ezmx.3: index backing store.FindingsByFileWindow, the same-file line-window
-- lookup triage's durable cross-lens fold now uses (widened from the exact
-- locus_key/OPEN-only point lookup). (file, line) lets the query filter to a
-- handful of rows per candidate instead of a table scan; status is left out
-- of the index because FindingsByFileWindow's status set varies by caller and
-- idx_findings_status already covers status-only queries.
CREATE INDEX idx_findings_file_line ON findings(file, line);
