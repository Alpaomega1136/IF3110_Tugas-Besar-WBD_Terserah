-- Development/demo database, separate from platform metadata.
CREATE TABLE products (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL,
    price numeric(12,2) NOT NULL CHECK (price >= 0)
);
CREATE INDEX products_name_idx ON products(name);
INSERT INTO products(name, price) VALUES ('Notebook', 25000), ('Pen', 5000);

