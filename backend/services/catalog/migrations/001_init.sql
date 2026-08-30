-- Catalog: identity (dealerships, opening hours, service types, customers, vehicles).

CREATE TABLE dealership (
    id       uuid PRIMARY KEY,
    name     text NOT NULL,
    timezone text NOT NULL
);

CREATE TABLE opening_hours (
    dealership_id uuid NOT NULL REFERENCES dealership (id),
    weekday       smallint NOT NULL CHECK (weekday BETWEEN 1 AND 7),
    open_minutes  int  NOT NULL CHECK (open_minutes >= 0 AND open_minutes < 1440),
    close_minutes int  NOT NULL CHECK (close_minutes > 0 AND close_minutes <= 1440),
    PRIMARY KEY (dealership_id, weekday)
);

CREATE TABLE service_type (
    id               uuid PRIMARY KEY,
    name             text NOT NULL,
    duration_minutes int  NOT NULL CHECK (duration_minutes >= 1)
);

CREATE TABLE customer (
    id   uuid PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE vehicle (
    id          uuid PRIMARY KEY,
    customer_id uuid NOT NULL REFERENCES customer (id),
    vin         text NOT NULL UNIQUE
);

CREATE INDEX opening_hours_dealership_idx ON opening_hours (dealership_id);
CREATE INDEX vehicle_customer_idx ON vehicle (customer_id);