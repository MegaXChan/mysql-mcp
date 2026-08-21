package database

import (
	"database/sql"
	"encoding/base64"
	"testing"
	"time"
)

func TestEncodeCellCoversStableScalarRepresentations(t *testing.T) {
	// Scenario: database/sql drivers may expose equivalent MySQL values through
	// several concrete Go scalar types. Risk covered: every accepted form has a
	// deterministic type tag and precision-preserving wire value.
	when := time.Date(2026, 8, 21, 10, 11, 12, 13, time.UTC)
	tests := []struct {
		name       string
		input      any
		dbType     string
		wantType   CellType
		wantValue  any
		wantEncode string
	}{
		{name: "null", input: nil, wantType: CellNull, wantValue: nil},
		{name: "bool", input: true, wantType: CellBoolean, wantValue: true},
		{name: "int", input: int(-1), wantType: CellInteger, wantValue: "-1"},
		{name: "int8", input: int8(-2), wantType: CellInteger, wantValue: "-2"},
		{name: "int16", input: int16(-3), wantType: CellInteger, wantValue: "-3"},
		{name: "int32", input: int32(-4), wantType: CellInteger, wantValue: "-4"},
		{name: "int64", input: int64(-5), wantType: CellInteger, wantValue: "-5"},
		{name: "uint", input: uint(1), wantType: CellInteger, wantValue: "1"},
		{name: "uint8", input: uint8(2), wantType: CellInteger, wantValue: "2"},
		{name: "uint16", input: uint16(3), wantType: CellInteger, wantValue: "3"},
		{name: "uint32", input: uint32(4), wantType: CellInteger, wantValue: "4"},
		{name: "uint64", input: ^uint64(0), wantType: CellInteger, wantValue: "18446744073709551615"},
		{name: "float32", input: float32(1.25), wantType: CellFloat, wantValue: "1.25"},
		{name: "float64", input: float64(2.5), wantType: CellFloat, wantValue: "2.5"},
		{name: "time", input: when, wantType: CellTime, wantValue: when.Format(time.RFC3339Nano)},
		{name: "string integer", input: "9", dbType: "BIGINT UNSIGNED", wantType: CellInteger, wantValue: "9"},
		{name: "string decimal", input: "1.2300", dbType: "DECIMAL(10,4)", wantType: CellDecimal, wantValue: "1.2300"},
		{name: "string float", input: "1.5", dbType: "DOUBLE", wantType: CellFloat, wantValue: "1.5"},
		{name: "string time", input: "12:34:56", dbType: "TIME", wantType: CellTime, wantValue: "12:34:56"},
		{name: "string text", input: "hello", dbType: "VARCHAR", wantType: CellString, wantValue: "hello"},
		{name: "bytes integer", input: []byte("10"), dbType: "INT", wantType: CellInteger, wantValue: "10"},
		{name: "bytes decimal", input: []byte("2.00"), dbType: "NUMERIC", wantType: CellDecimal, wantValue: "2.00"},
		{name: "bytes float", input: []byte("3.5"), dbType: "FLOAT", wantType: CellFloat, wantValue: "3.5"},
		{name: "bytes time", input: []byte("2026-08-21"), dbType: "DATE", wantType: CellTime, wantValue: "2026-08-21"},
		{name: "bytes text", input: []byte("json"), dbType: "JSON", wantType: CellString, wantValue: "json"},
		{name: "raw bytes", input: sql.RawBytes{0xff, 0x00}, dbType: "BLOB", wantType: CellBytes, wantValue: base64.StdEncoding.EncodeToString([]byte{0xff, 0x00}), wantEncode: "base64"},
		{name: "invalid utf8 text", input: []byte{0xff}, dbType: "TEXT", wantType: CellBytes, wantValue: "/w==", wantEncode: "base64"},
		{name: "geometry point is binary", input: []byte{1, 2}, dbType: "POINT", wantType: CellBytes, wantValue: "AQI=", wantEncode: "base64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cell, sourceBytes, encodedBytes, err := encodeCell(test.input, test.dbType)
			if err != nil {
				t.Fatalf("encodeCell() error = %v", err)
			}
			if cell.Type != test.wantType || cell.Value != test.wantValue || cell.Encoding != test.wantEncode {
				t.Fatalf("encodeCell() = %#v, want type=%q value=%#v encoding=%q", cell, test.wantType, test.wantValue, test.wantEncode)
			}
			if sourceBytes < 0 || encodedBytes < 0 {
				t.Fatalf("encodeCell() sizes = %d/%d, want non-negative", sourceBytes, encodedBytes)
			}
		})
	}
}

func TestEncodeCellRejectsUnknownDriverType(t *testing.T) {
	// Scenario: a custom driver returns a value outside database/sql's documented
	// scalar set. Risk covered: the encoder fails closed instead of serializing an
	// unstable fmt-based representation.
	if _, _, _, err := encodeCell(struct{ Secret string }{Secret: "x"}, "CUSTOM"); err == nil {
		t.Fatal("encodeCell() error = nil, want unsupported-type error")
	}
}

func TestIntegerTypeMatchingDoesNotMisclassifyPoint(t *testing.T) {
	// Scenario: the geometry type POINT happens to contain the letters "INT".
	// Risk covered: type classification uses complete MySQL names, not substring
	// matching that would emit binary geometry as an integer.
	if isIntegerType("POINT") {
		t.Fatal("isIntegerType(POINT) = true, want false")
	}
	if !isIntegerType("UNSIGNED BIGINT") || !isIntegerType("BIGINT UNSIGNED") {
		t.Fatal("isIntegerType() did not recognize unsigned BIGINT spelling")
	}
}
