-- Paste into a new migration draft on a dedicated demo target.
-- Use an unused schema name when repeating the demo; Execute owns the transaction.
CREATE SCHEMA wbd_demo;
CREATE TABLE wbd_demo.items (
    id bigint PRIMARY KEY,
    amount numeric(20,4),
    note text
);
INSERT INTO wbd_demo.items VALUES
    (9007199254740993, 123.4500, 'semi;colon'),
    (9007199254740994, 0, NULL);
