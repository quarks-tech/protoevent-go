CREATE TABLE outbox (
    id           BIGINT       NOT NULL AUTO_INCREMENT,
    seq          BIGINT       NULL,
    tx_start_ts  BIGINT       NOT NULL,
    event_id     BINARY(16)   NOT NULL,
    metadata     JSON         NOT NULL,
    data         MEDIUMBLOB   NOT NULL,
    create_time  DATETIME(6)  NOT NULL,
    occurred_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id) /*T![clustered_index] CLUSTERED */,
    UNIQUE KEY uk_outbox_event (event_id),
    KEY idx_outbox_seq (seq, tx_start_ts)
);

CREATE TABLE outbox_sequencer (
    name     VARCHAR(64) NOT NULL,
    next_seq BIGINT      NOT NULL,
    PRIMARY KEY (name)
);

INSERT INTO outbox_sequencer (name, next_seq) VALUES ('default', 1);

CREATE TABLE outbox_offsets (
    name        VARCHAR(64) NOT NULL,
    last_seq    BIGINT      NOT NULL,
    update_time DATETIME(6) NOT NULL,
    PRIMARY KEY (name)
);

CREATE TABLE relay_lock (
    name        VARCHAR(64) NOT NULL,
    holder_id   VARCHAR(64) NOT NULL,
    expire_time DATETIME(6) NOT NULL,
    PRIMARY KEY (name)
);
