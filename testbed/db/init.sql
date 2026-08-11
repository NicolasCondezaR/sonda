-- The bookshop's whole database. Run once by the postgres image on first start.
--
-- `members.password` is stored in the clear, which no real system should do.
-- It is here because the login query has to name a credential for Sonda's
-- Postgres redaction to have something to blank, and a hash would not.

CREATE TABLE books (
    sku          text PRIMARY KEY,
    title        text    NOT NULL,
    author       text    NOT NULL,
    price_cents  integer NOT NULL,
    discount_pct integer NOT NULL DEFAULT 0,
    in_stock     boolean NOT NULL DEFAULT true
);

CREATE TABLE reviews (
    id    serial PRIMARY KEY,
    sku   text    NOT NULL REFERENCES books (sku),
    stars integer NOT NULL,
    body  text    NOT NULL
);

CREATE TABLE members (
    email    text PRIMARY KEY,
    name     text NOT NULL,
    password text NOT NULL
);

INSERT INTO books (sku, title, author, price_cents, discount_pct, in_stock) VALUES
    ('DUNE',        'Dune',                     'Frank Herbert',    1890, 10, true),
    ('PALE-FIRE',   'Pale Fire',                'Vladimir Nabokov', 1450,  0, true),
    ('SOLARIS',     'Solaris',                  'Stanislaw Lem',    1220,  5, true),
    ('RESTRICTED-1','The Librarian''s Ledger',  'Anonymous',        9900,  0, true),
    ('OUT-OF-PRINT','Codex Seraphinianus',      'Luigi Serafini',   7800,  0, false);

INSERT INTO reviews (sku, stars, body) VALUES
    ('DUNE',      5, 'The spice must flow.'),
    ('DUNE',      4, 'Long, but worth it.'),
    ('PALE-FIRE', 5, 'A poem and its footnotes, at war.'),
    ('SOLARIS',   4, 'The ocean is the point.');

INSERT INTO members (email, name, password) VALUES
    ('ada@bookshop.test',  'Ada Lovelace',  'shelf-of-books'),
    ('borges@bookshop.test','Jorge Borges', 'the-garden-of-forking-paths');
