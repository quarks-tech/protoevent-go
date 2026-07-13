package tidb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// These are pure unit tests for the error classifiers — no database involved.
// The live counterpart of isNullColumn's happy path is
// TestCreateOutboxMessageRejectsAutocommit (store_test.go), which proves a
// real autocommit publish raises exactly 1048 on tx_start_ts.

func TestIsDuplicateKeyTrueFor1062(t *testing.T) {
	err := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'outbox_offsets.PRIMARY'"}
	if !isDuplicateKey(err) {
		t.Fatal("isDuplicateKey(1062) = false, want true")
	}
}

func TestIsDuplicateKeyFalseForOtherNumber(t *testing.T) {
	err := &mysql.MySQLError{Number: 1048, Message: "Column 'tx_start_ts' cannot be null"}
	if isDuplicateKey(err) {
		t.Fatal("isDuplicateKey(1048) = true, want false")
	}
}

func TestIsDuplicateKeyFalseForNonMySQLError(t *testing.T) {
	if isDuplicateKey(errors.New("Duplicate entry")) {
		t.Fatal("isDuplicateKey(non-MySQLError) = true, want false")
	}
}

func TestIsDuplicateKeyWrapped(t *testing.T) {
	err := fmt.Errorf("outbox: init offset latest: %w",
		&mysql.MySQLError{Number: 1062, Message: "Duplicate entry"})
	if !isDuplicateKey(err) {
		t.Fatal("isDuplicateKey(wrapped 1062) = false, want true")
	}
}

func TestIsNullColumnTrueForMatchingColumn(t *testing.T) {
	// The genuine autocommit rejection: NULLIF(@@tidb_current_ts, 0) turned the
	// autocommit 0 into NULL and the NOT NULL column rejected the row.
	err := &mysql.MySQLError{Number: 1048, Message: "Column 'tx_start_ts' cannot be null"}
	if !isNullColumn(err, "tx_start_ts") {
		t.Fatal("isNullColumn(1048 on tx_start_ts) = false, want true")
	}
}

func TestIsNullColumnFalseForOtherColumn(t *testing.T) {
	// 1048 on a different NOT NULL column (e.g. nil Data) must NOT be labeled
	// as the autocommit rejection.
	err := &mysql.MySQLError{Number: 1048, Message: "Column 'data' cannot be null"}
	if isNullColumn(err, "tx_start_ts") {
		t.Fatal("isNullColumn(1048 on data) = true, want false")
	}
}

func TestIsNullColumnFalseForOtherNumber(t *testing.T) {
	// Schema drift mentioning the column (Error 1054 unknown column) is not an
	// autocommit rejection and must not be misdiagnosed as one.
	err := &mysql.MySQLError{Number: 1054, Message: "Unknown column 'tx_start_ts' in 'field list'"}
	if isNullColumn(err, "tx_start_ts") {
		t.Fatal("isNullColumn(1054) = true, want false")
	}
}

func TestIsNullColumnFalseForNonMySQLError(t *testing.T) {
	if isNullColumn(errors.New("Column 'tx_start_ts' cannot be null"), "tx_start_ts") {
		t.Fatal("isNullColumn(non-MySQLError) = true, want false")
	}
}

func TestIsNullColumnWrapped(t *testing.T) {
	err := fmt.Errorf("wrapped: %w",
		&mysql.MySQLError{Number: 1048, Message: "Column 'tx_start_ts' cannot be null"})
	if !isNullColumn(err, "tx_start_ts") {
		t.Fatal("isNullColumn(wrapped 1048) = false, want true")
	}
}
