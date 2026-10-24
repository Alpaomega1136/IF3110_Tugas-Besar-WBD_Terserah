-- Run after 001_create.sql using the same demo schema.
-- The division error must roll back both the new table and the inserted row.
CREATE TABLE wbd_demo.rollback_proof(id int);
INSERT INTO wbd_demo.items VALUES (3, 1, 'rollback');
SELECT 1/0;
